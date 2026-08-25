// Package mcp — the `ask` tool.
//
// context.go wires up `ask`, a query planner for code understanding.
// The goal is to be the single entry point an agent reaches for
// instead of fanning out to grep / Read / search_semantic loops.
// Given a project and a free-text question (plus optional intent
// override), the router picks a strategy, runs the right combination
// of legs (search_semantic, search_symbol, graph queries) and
// returns a compact bundle with `suggested_reads`, a prose
// `next_action`, and an `avoid` line.
//
// Graph integration: callers/callees use the `calls` edges from
// internal/graph (Go-only). Other languages get a BM25 chunk search
// over the bare symbol name as a fallback (see runReferencesLane).
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/alehatsman/dex/internal/retrieve"
	"github.com/alehatsman/dex/internal/store"
	"github.com/alehatsman/dex/internal/throttle"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ─── tool: ask ────────────────────────────────────────────────────────────

type ContextInput struct {
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project or git worktree you are working in. The server cannot see your shell's directory; when working in a worktree different from where the server started, pass that worktree's path"`
	Question    string `json:"question" jsonschema:"free-text question about the codebase (e.g. 'where is filesystem event debouncing handled?', 'how does indexing work?', 'callers of (*Store).Search')"`
	Intent      string `json:"intent,omitempty" jsonschema:"force a strategy: auto|behavior_search|symbol_lookup|callers|callees|architecture|package_topology|editing_context|assemble|review (default: auto). review returns a delta-shaped code review of your working-tree changes in the review field (#144); for targeted PR/branch/ref review use the review_diff tool. assemble returns a budget-bounded working set — symbol bodies chosen by submodular keyword coverage, prose synthesis suppressed — instead of a prose answer (#687); the concerns field reports which query concerns the set covers vs drops, so a partial set isn't read as complete (#725)"`
	K           int    `json:"k,omitempty" jsonschema:"max hits per lane (default 8, max 30)"`
	NoInline    bool   `json:"no_inline,omitempty" jsonschema:"skip inlining file contents into suggested_reads and semantic_hits. Default off: both lanes carry their line-range content from one shared per-intent byte pool (per-range cap ~60 lines / 4 KB; total cap ~20 KB targeted / ~40 KB exploration; oversize ranges are clipped with truncated=true). Set true if you already have the files open, or in long sessions where context budget is limited — check content_bytes_inlined from a prior ask to gauge how much was consumed."`
	Expand      string `json:"expand,omitempty" jsonschema:"opt-in query-side expansion (#252): off|on|full. on adds model-generated keywords+identifiers to the BM25 and symbol lanes (no extra embedding); full also embeds a hypothetical-answer passage into the vector lane. Empty defers to the server default (DEX_EXPAND_MODE). Requires DEX_EXPAND_MODEL to be configured; otherwise a no-op."`
	AnswerStyle string `json:"answer_style,omitempty" jsonschema:"controls chat synthesis: brief runs the LLM synthesis leg and returns an answer field; none skips synthesis entirely and returns only the evidence bundle. Default is none — pass brief to get a synthesized answer."`
	Budget      int    `json:"budget,omitempty" jsonschema:"optional context-token budget; when set, the response reports cost.budget_left = budget − tokens_returned (cost.tokens_returned is always reported)"`
}

// SemHit is a semantic-search result reduced to the wire shape the
// issue specifies. Content is inlined by default so the caller doesn't
// have to issue a follow-up Read for hits below the suggested_reads
// cut; the same per-intent budget pool covers both lanes (see
// inlineCapsFor / inlineContent). Empty when no_inline=true, when the
// file cannot be opened, or when the shared byte budget was exhausted
// before this hit.
type SemHit struct {
	Path      string  `json:"path"`
	StartLine int     `json:"start_line"`
	EndLine   int     `json:"end_line"`
	Score     float32 `json:"score"`
	Kind      string  `json:"kind,omitempty"`
	Reason    string  `json:"reason,omitempty"`
	// Lanes names the retrieval lanes that surfaced this hit — any of
	// "vector", "bm25", "graph" (#707). A multi-lane hit is higher-confidence
	// than a single-lane one; read those first rather than trusting list
	// position. Pure provenance — it never reorders results.
	Lanes     []string `json:"lanes,omitempty"`
	Content   string   `json:"content,omitempty"`
	Truncated bool     `json:"truncated,omitempty"`
	// Handle is the opaque expansion handle for this hit's range (#344).
	Handle string `json:"handle,omitempty"`
	// SeenTurn is set (>0) when this exact range was already surfaced to the
	// session on an earlier turn (#344); content is then omitted to save bytes.
	SeenTurn int `json:"seen,omitempty"`
}

type SymbolHit struct {
	QualifiedName string `json:"qualified_name"`
	Path          string `json:"path,omitempty"`
	StartLine     int    `json:"start_line,omitempty"`
	EndLine       int    `json:"end_line,omitempty"`
	Kind          string `json:"kind,omitempty"`
	// Signature is the declaration line (e.g. `func (s *Store) Search(q
	// string) ([]Hit, error)`). Cheap: one file line at StartLine. Lets
	// the caller see the API contract without reading the body.
	Signature string `json:"signature,omitempty"`
	// Doc is the contiguous comment block immediately above StartLine
	// (Go `//` lines, Python `#` lines). Capped at ~10 lines / 600 B.
	Doc string `json:"doc,omitempty"`
	// Body is the symbol's full source between StartLine and EndLine,
	// populated only for symbol_lookup intent (the case where the caller
	// almost always wants to read the body after seeing the signature).
	// Shares the per-intent inline byte budget with suggested_reads /
	// semantic_hits via inlineContent; oversized symbols are clipped at
	// the per-range cap with Truncated=true.
	Body string `json:"body,omitempty"`
	// Role is the compact call-graph tag (e.g. "central:13/2pkg",
	// "exported-unused", "leaf") composed from centrality columns by
	// formatRole. Empty when the symbol has no graph node (non-Go file,
	// missing graph) or sits in the unremarkable middle. Mirrors the
	// Role field on SearchHit and CallSite so all symbol surfaces look
	// the same to an agent.
	Role      string `json:"role,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	// Handle is the opaque expansion handle for this symbol's range (#344).
	Handle string `json:"handle,omitempty"`
	// SeenTurn is set (>0) when this exact range was already surfaced to the
	// session on an earlier turn (#344); content is then omitted to save bytes.
	SeenTurn int `json:"seen,omitempty"`
}

// RefHit is a reference produced by the references lane
// (callers/callees intents). Stand-in for the deferred `calls` graph
// edges — BM25 chunk search over the bare symbol name, capped to a few
// dozen hits. The definition chunk is filtered out so the list is genuinely
// "uses of" rather than "appearances of".
type RefHit struct {
	Path    string `json:"path"`
	Line    int    `json:"line"`
	Snippet string `json:"snippet,omitempty"` // single-line excerpt
	Symbol  string `json:"symbol,omitempty"`  // which symbol this is a ref to
}

// PathMeta is the per-file annotation bundle keyed by relative path in
// ContextOutput.Annotations. Fields are populated conditionally based
// on intent and may individually be empty. Designed so all data about
// a single file lives in one place — the caller joins by path.
type PathMeta struct {
	// LastCommit / LastAuthor are populated for editing_context. Short
	// SHA + short date + author; e.g. "5a79083 2026-05-19 Aleh Atsman".
	LastCommit string `json:"last_commit,omitempty"`
	LastAuthor string `json:"last_author,omitempty"`
	// Owners from the project's CODEOWNERS file, matched by glob.
	// Populated for editing_context only.
	Owners []string `json:"owners,omitempty"`
	// NearestDoc is the closest documentation file walking up from the
	// path's directory — CLAUDE.md > doc.go > README.md, stopping at
	// projectRoot. Always-on (cheap dir walk).
	NearestDoc string `json:"nearest_doc,omitempty"`
	// Tests are sibling test files paired by language convention
	// (foo.go ↔ foo_test.go; foo.py ↔ test_foo.py; foo.ts ↔
	// foo.test.ts). Always-on (pure path heuristic).
	Tests []string `json:"tests,omitempty"`
	// BuildTags is the //go:build or // +build constraint line plus the
	// package clause for Go files; populated for editing_context,
	// architecture, and package_topology.
	BuildTags string `json:"build_tags,omitempty"`
	// Package is the `package x` clause for Go files; populated
	// alongside BuildTags.
	Package string `json:"package,omitempty"`
}

// GraphResult is the placeholder for the deferred graph layer. Always
// emitted (even when empty) so the caller can rely on the field
// existing — when graph lands the wire shape grows but the field
// presence doesn't change.
type GraphResult struct {
	Nodes []GraphNode `json:"nodes"`
	Edges []GraphEdge `json:"edges"`
}

type GraphNode struct {
	ID            string `json:"id"`
	QualifiedName string `json:"qualified_name,omitempty"`
	Kind          string `json:"kind,omitempty"`
	// Import-graph centrality — populated only by the package_topology lane so
	// an agent can rank the load-bearing packages in one call (#190). omitempty
	// keeps every other intent's nodes at the old {id,kind} shape.
	InDegree  int     `json:"in_degree,omitempty"`
	OutDegree int     `json:"out_degree,omitempty"`
	PageRank  float64 `json:"page_rank,omitempty"`
}

type GraphEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind,omitempty"`
}

type SuggestedRead struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line,omitempty"`
	EndLine   int    `json:"end_line,omitempty"`
	Reason    string `json:"reason"`
	// Content is the file slice for [StartLine, EndLine], inlined by
	// default so the caller doesn't need a follow-up Read for the
	// common case. Capped per-read and totaled across reads — see
	// inlineSuggestedReads. Empty when no_inline=true, when the file
	// cannot be opened, or when the caller hit the total byte budget.
	Content string `json:"content,omitempty"`
	// Truncated is set when the per-read line/byte cap clipped the
	// content before reaching EndLine. The caller can still issue a
	// regular Read for the rest if needed.
	Truncated bool `json:"truncated,omitempty"`
	// Imports is the file's import block (Go `import (...)` / single-line
	// imports, Python `import` / `from import`, JS/TS `import` /
	// `require(...)`). Inlined per-file once across the bundle so the
	// caller sees what the file depends on without a separate Read of
	// the first 30 lines. Empty when the language isn't supported, the
	// file has no imports, the StartLine range already covers the
	// imports, or the shared byte budget is exhausted.
	Imports string `json:"imports,omitempty"`
	// Handle is the opaque expansion handle for this read's range (#344).
	Handle string `json:"handle,omitempty"`
	// SeenTurn is set (>0) when this exact range was already surfaced to the
	// session on an earlier turn (#344); content is then omitted to save bytes.
	SeenTurn int `json:"seen,omitempty"`
}

type ContextOutput struct {
	Status string `json:"status"` // ok | no-index | index-empty | embedding-service-unreachable | error
	Hint   string `json:"hint,omitempty"`
	// Answer is the synthesized prose response to the question, grounded
	// in the evidence below and citing `path:line`. Populated when a chat
	// client is configured and reachable; absent on degradation, in which
	// case the agent works from the evidence bundle + next_action.
	Answer string `json:"answer,omitempty"`
	// AnswerModel names the model that produced Answer (e.g.
	// "qwen2.5-coder:14b"). Empty when no answer was synthesized.
	AnswerModel string `json:"answer_model,omitempty"`
	Endpoint    string `json:"endpoint,omitempty"` // populated when embed is unreachable
	Project     string `json:"project,omitempty"`
	Intent      string `json:"intent,omitempty"`
	// Trust is the single confidence/freshness envelope for ask (#95c/#116):
	// index freshness (stale/indexing/indexed_at), the "high|medium|low"
	// confidence self-assessment, and evidence-derived signals (top score,
	// low-confidence, call-graph resolution, recall caveat) the Assembler
	// computes once. omitempty; present on every real ask, nil only on the
	// no-lane early return. Supersedes the former top-level stale/indexing/
	// confidence fields.
	Trust          *EnvTrust       `json:"trust,omitempty"`
	SemanticHits   []SemHit        `json:"semantic_hits,omitempty"`
	Symbols        []SymbolHit     `json:"symbols,omitempty"`
	Graph          *GraphResult    `json:"graph,omitempty"`
	SuggestedReads []SuggestedRead `json:"suggested_reads,omitempty"`
	NextAction     string          `json:"next_action,omitempty"`
	Avoid          string          `json:"avoid,omitempty"`
	// Next is the structured form of the follow-up move, so the two-verb
	// surface (#195) exposes one `next` key across query/record. It
	// is derived from evidence query already produced (the top suggested read),
	// never re-inferred; NextAction stays as the canonical prose rationale.
	// Absent when there is no concrete grounded step. ask keeps its richer
	// trust / next_action — this only aligns the key name.
	Next []NextStep `json:"next,omitempty"`
	// Cost is the uniform token envelope (#110 step 2): tokens_returned is
	// stamped on every response; budget_left appears when the caller passed a
	// budget. content_bytes_inlined stays as ask's byte-level inline gauge.
	Cost *EnvCost `json:"cost,omitempty"`
	// Map is the deterministic L0+L1 orientation bundle, set only on the
	// session-start orientation path (ask called with an empty question, #348 /
	// #316 story 6). Zero inference, byte-stable across calls. Absent otherwise.
	Map string `json:"map,omitempty"`
	// References is the BM25-backed reference list. Populated for
	// callers/callees intents when at least one SymbolHit is present.
	// Stand-in for the deferred `calls` graph edges.
	References []RefHit `json:"references,omitempty"`
	// Annotations is per-file metadata keyed by the same relative path
	// used in SuggestedReads / Symbols / SemanticHits. Which sub-fields
	// are populated depends on intent (see enrich.go for the gating
	// matrix). Callers join by path.
	Annotations map[string]PathMeta `json:"annotations,omitempty"`
	// SessionTask is the current session's declared task, if any.
	SessionTask string `json:"session_task,omitempty"`
	// KnowledgeFacts are the top project facts ordered by salience,
	// injected from the knowledge base so agents see accumulated context.
	KnowledgeFacts []string `json:"knowledge_facts,omitempty"`
	// ContentBytesInlined is the total bytes of file content inlined into
	// suggested_reads and semantic_hits. Zero when no_inline=true or when
	// no content was inlined. Use this to gauge context-window cost and
	// decide whether to pass no_inline=true on follow-up calls.
	ContentBytesInlined int `json:"content_bytes_inlined,omitempty"`
	// Expanded reports that query-side expansion (#252) contributed terms to
	// this answer's lanes. Off unless DEX_EXPAND_MODEL is configured and the
	// expansion call returned something usable.
	Expanded bool `json:"expanded,omitempty"`
	// RelatedFiles are files surfaced by spreading activation over the union of
	// the static call/import graph and learned co-access (Hebbian) edges (#688),
	// seeded on the assemble working set. Populated only for intent=assemble.
	RelatedFiles []string `json:"related_files,omitempty"`
	// Rules are the project's local rule/spec files (CLAUDE.md, .dex/rules.md,
	// docs/*.md, specs/*.md) that govern edits to the working set — the
	// constraints an agent must honour before touching code. Folded in from
	// brief (#141) so intent=assemble is a complete task-start pack. Populated
	// only for intent=assemble.
	Rules []string `json:"rules,omitempty"`
	// Concerns is the assemble completeness signal (#725): which query
	// concerns the inlined working set is ABOUT (covered) vs which the byte
	// budget dropped (no inlined symbol body is about them). It exists so a
	// partial set isn't mistaken for complete — an honest partial beats a
	// false floor. Populated only for intent=assemble with coverage keys.
	Concerns *AssembleConcerns `json:"concerns,omitempty"`
	// Review is the delta-shaped result of intent=review (#144, ask-merge slice
	// 5a). Code review is a diff-scoped delta, not the state-shaped evidence the
	// lanes above carry, so it rides its own discriminated-union field: when this
	// is set the state lanes are empty, and vice versa. Populated only when the
	// router picks IntentReview ("review my changes"); the auto path reviews the
	// working tree. Targeted PR/branch/ref review stays on review_diff / the CLI.
	Review *ReviewOutput `json:"review,omitempty"`
}

// AssembleConcerns reports the per-concern completeness of an assemble
// working set (#725). A concern keyword is Covered when some symbol whose
// body was actually inlined is about it, and Dropped otherwise. Both lists
// preserve coverage-key order.
type AssembleConcerns struct {
	Covered []string `json:"covered,omitempty"`
	Dropped []string `json:"dropped,omitempty"`
}

// ContextRouter is the exported entry point used by the CLI
// (`dex ask`). It delegates to the MCP-registered handler.
func (s *Server) ContextRouter(ctx context.Context, in ContextInput) (*sdk.CallToolResult, ContextOutput, error) {
	return s.contextRouterStream(ctx, nil, in, nil)
}

// ContextRouterStream is ContextRouter with a token sink: when tokenSink
// is non-nil, answer-synthesis tokens are delivered to it as they arrive
// (the CLI streams them to stdout for a fast first token). A nil sink is
// identical to ContextRouter.
func (s *Server) ContextRouterStream(ctx context.Context, in ContextInput, tokenSink func(string)) (*sdk.CallToolResult, ContextOutput, error) {
	return s.contextRouterStream(ctx, nil, in, tokenSink)
}

// contextRouter satisfies the toolSurface interface used by tool
// registration and the HTTP/remote proxies; those paths never stream, so
// it delegates with a nil sink.
func (s *Server) contextRouter(ctx context.Context, req *sdk.CallToolRequest, in ContextInput) (*sdk.CallToolResult, ContextOutput, error) {
	return s.contextRouterStream(ctx, req, in, nil)
}

// contextRouterCheckStale sets the freshness hint on out and returns the facts
// (stale, indexing, and the index's last-indexed time — zero if unknown) for the
// Trust envelope (#95c/#116). The booleans ride pack.Trust rather than top-level
// out fields, so the router threads them into the AssembleRequest.
func contextRouterCheckStale(ctx context.Context, st *store.Store, out *ContextOutput, root string) (stale, indexing bool, indexedAt time.Time) {
	if stats, statsErr := st.Stats(ctx); statsErr == nil && !stats.LastIndex.IsZero() {
		indexedAt = stats.LastIndex
		if time.Since(stats.LastIndex) > 24*time.Hour {
			stale = true
			out.Hint = appendHint(out.Hint, fmt.Sprintf("index is %s old — run `dex index %s` to refresh.",
				time.Since(stats.LastIndex).Round(time.Hour), root))
		}
	}
	// An active rebuild trumps age: evidence is being rewritten right now, so
	// what we return is partial (#531).
	if inProgress, note := indexingNotice(ctx, st); inProgress {
		stale = true
		indexing = true
		out.Hint = note
	}
	return stale, indexing, indexedAt
}

// loadContextFacts loads the session task and knowledge facts into out.
func (s *Server) loadContextFacts(ctx context.Context, st *store.Store, in ContextInput, out *ContextOutput) {
	if ss, ok, err := st.SessionGet(ctx); err == nil && ok && ss.Task != "" {
		out.SessionTask = ss.Task
	}
	// skipFallback=true routes ask through the 0.5 relevance floor (KnowledgeQueryVec
	// strict mode) and drops the top-salience fallback: a query injects only facts
	// that actually match it, not whatever scores highest by archetype weight. This
	// keeps off-topic, high-salience notes (e.g. bulky VerifiedFact session logs that
	// cosine ~0.3-0.4) out of every unrelated ask (#785).
	if facts, err := s.recallFacts(ctx, st, in.Question, 5, true, "", true); err == nil && len(facts) > 0 {
		for _, f := range facts {
			out.KnowledgeFacts = append(out.KnowledgeFacts, "["+f.Archetype+"] "+capFactBody(f.Body))
		}
	}
}

// maxInjectedFactBody bounds how much of one fact body ask inlines into
// knowledge_facts. The relevance floor keeps off-topic facts out, but an
// on-topic giant (a multi-KB note body) would still blow the response budget,
// so each injected body is clipped to this many runes (#785).
const maxInjectedFactBody = 600

// capFactBody clips an over-long fact body for injection, appending a marker so
// the truncation is visible. Operates on runes to avoid splitting a multibyte
// character.
func capFactBody(body string) string {
	r := []rune(body)
	if len(r) <= maxInjectedFactBody {
		return body
	}
	return strings.TrimRight(string(r[:maxInjectedFactBody]), " ") + " …(truncated)"
}

// contextRouterStream is the single chokepoint all ask entry points (MCP tool,
// CLI, HTTP) funnel through. It runs the router, then stamps the structured
// `next` step so every path — success, degraded, orient, and error — carries the
// four-verb envelope's `next` key without touching the router's many internal
// returns.
func (s *Server) contextRouterStream(ctx context.Context, req *sdk.CallToolRequest, in ContextInput, tokenSink func(string)) (*sdk.CallToolResult, ContextOutput, error) {
	res, out, err := s.contextRouterStreamImpl(ctx, req, in, tokenSink)
	deriveAskNext(&out)
	return res, out, err
}

// deriveAskNext fills ContextOutput.Next from evidence ask already produced. The
// grounded, non-inferred follow-up is to look at the top suggested read — ask has
// already decided that file is worth opening. It never overwrites a Next the
// router set explicitly, and emits nothing when there is no concrete target.
func deriveAskNext(out *ContextOutput) {
	if len(out.Next) > 0 || len(out.SuggestedReads) == 0 {
		return
	}
	top := out.SuggestedReads[0]
	if strings.TrimSpace(top.Path) == "" {
		return
	}
	target := top.Path
	if top.StartLine > 0 {
		target = top.Path + ":" + strconv.Itoa(top.StartLine)
	}
	why := "open the top suggested read"
	if strings.TrimSpace(top.Reason) != "" {
		why = top.Reason
	}
	out.Next = []NextStep{{
		Verb: "look",
		Args: map[string]any{"target": target},
		Why:  why,
	}}
}

func (s *Server) contextRouterStreamImpl(ctx context.Context, req *sdk.CallToolRequest, in ContextInput, tokenSink func(string)) (*sdk.CallToolResult, ContextOutput, error) {
	if strings.TrimSpace(in.Question) == "" {
		// Empty question = session-start orientation: return the deterministic
		// L0+L1 map so the agent names the right cluster before any find()
		// (#348 / #316 story 6). No inference, byte-stable, cache-friendly.
		return s.orientResponse(ctx, in)
	}
	p, hint := s.resolveProject(ctx, in.ProjectRoot)
	if hint != "" {
		return nil, ContextOutput{Status: "error", Hint: hint}, nil
	}

	// Repetition guard: at 7+ identical asks skip the expensive search+LLM
	// pipeline and return just the hint. At 4–6, continue but annotate.
	if throttleHint, earlyReturn := s.searchThrottleHint(in.Question, p.Root); earlyReturn {
		return nil, ContextOutput{Status: "ok", Project: p.Root, Hint: throttleHint}, nil
	} else if throttleHint != "" {
		// Pre-set hint; activityNudge will be skipped below in favour of this.
		hint = throttleHint
	}

	intent, candidates := retrieve.ResolveIntent(in.Question, in.Intent)
	if intent == retrieve.IntentOrient {
		// #135: a whole-repo orientation question ("understand this repo",
		// "overview of the codebase") gets the same deterministic orient bundle
		// as an empty question — the L0/L1 map + build/test commands answer it
		// better than semantic search + LLM synthesis. The classifier keeps this
		// narrow (explicit repo subject); an explicit non-orient intent override
		// still wins because ResolveIntent honours it first.
		return s.orientResponse(ctx, in)
	}
	if intent == retrieve.IntentReview {
		// #144: "review my changes" routes to the per-hunk review composition,
		// not the search lanes. Its result is delta-shaped, so reviewResponse
		// carries it in the discriminated-union ContextOutput.Review field. The
		// auto path reviews the working tree (Review's #137 default); targeted
		// PR/branch/ref review stays on review_diff / `dex review`. An explicit
		// non-review intent override still wins — ResolveIntent honours it first.
		return s.reviewResponse(ctx, in)
	}
	out := ContextOutput{Project: p.Root, Intent: intent}
	if hint != "" {
		out.Hint = hint
	}

	if _, err := os.Stat(p.DBPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			out.Status = "no-index"
			out.Hint = fmt.Sprintf("no index for %s — run `dex index %s` first; fall back to grep until then.", p.Root, p.Root)
			return nil, out, nil
		}
		out.Status = "error"
		out.Hint = err.Error()
		return nil, out, nil
	}

	k := in.K
	if k <= 0 {
		k = 8
	}
	k = min(k, 30)

	st, err := s.openStore(p.DBPath)
	if err != nil {
		out.Status = "error"
		out.Hint = fmt.Sprintf("open index: %v", err)
		return nil, out, nil
	}

	stale, indexing, indexedAt := contextRouterCheckStale(ctx, st, &out, p.Root)
	s.loadContextFacts(ctx, st, in, &out)

	// The assembler sets pack.Graph only when it has something to emit; the
	// projection below leaves out.Graph absent otherwise. An absent `graph`
	// key signals "no graph indexed, or this intent surfaced no structural
	// context" — saves bytes over shipping `{nodes:[], edges:[]}` on every
	// response.

	// Load the graph view once per request. Nil view = no graph
	// indexed; intents that need it will note this in `avoid`.
	graphView, _ := s.cachedLoadGraphView(ctx, st, p.DBPath)

	// Query-side expansion (#252) — opt-in, failure-soft. One small-model
	// call turns the question into extra retrieval terms fanned across the
	// lanes; the raw question stays in every lane so a fanciful generation
	// is diluted by RRF rather than amplified. Empty/timeout/error → the
	// un-expanded query. Placed after store-open so a broken repo never
	// pays the GPU call.
	exp := retrieve.ExpandQuery(ctx, s.ExpandClient, in.Question, retrieve.ResolveExpandMode(in.Expand, s.ExpandMode))
	if !exp.Empty() {
		candidates.Identifiers = retrieve.AppendExpansionIdentifiers(candidates.Identifiers, exp.Identifiers)
		out.Expanded = true
	}

	// Evidence assembly (#95a / #103) — the symbol/semantic lanes, graph
	// neighborhood, lane-agreement reweight and suggested-reads ranker now
	// compose in internal/retrieve.Assembler, producing a complete ContextPack.
	// The transport injects the policies it deliberately owns (the call-graph
	// role vocabulary and path classification, both shared across tools; the
	// byte-budget inline presentation pass; the feedback A/B hooks), then
	// projects the pack onto the wire response and applies transport concerns
	// (near-miss, session, throttle, answer synthesis, handles, dedup).
	pack, meta := retrieve.Assembler{
		Service:    retrieve.Service{Embed: s.EmbedClient},
		FormatRole: formatRole,
		IsNonImpl:  isNonImplPath,
		IsTestPath: isTestPath,
	}.Assemble(ctx, st, retrieve.AssembleRequest{
		Intent:       intent,
		Question:     in.Question,
		Candidates:   candidates,
		K:            k,
		Graph:        graphView,
		EmbedText:    retrieve.ExpandedEmbedText(in.Question, exp),
		FTSText:      retrieve.ExpandedFTSText(in.Question, exp),
		Expanded:     out.Expanded,
		ProjectRoot:  p.Root,
		NoInline:     in.NoInline,
		Spread:       st,
		RecordShadow: s.recordShadowPack,
		Reweight:     s.reweightPack,
		Stale:        stale,
		Indexing:     indexing,
		IndexedAt:    indexedAt,
	})
	out.Symbols = fromPackSyms(pack.Symbols)
	out.SemanticHits = fromPackSems(pack.SemanticHits)
	if g := fromPackGraph(pack.Graph); g != nil {
		out.Graph = g
	}
	out.SuggestedReads = fromPackReads(pack.SuggestedReads)
	out.References = fromNeutralRefs(pack.References)
	out.Annotations = fromNeutralAnnotations(pack.Annotations)
	out.RelatedFiles = pack.RelatedFiles
	out.ContentBytesInlined = pack.ContentBytesInlined
	if pack.Concerns.Covered != nil || pack.Concerns.Dropped != nil {
		out.Concerns = &AssembleConcerns{Covered: pack.Concerns.Covered, Dropped: pack.Concerns.Dropped}
	}
	out.NextAction = pack.NextAction
	out.Avoid = pack.Avoid
	out.Trust = fromPackTrust(pack.Trust)

	embedFailed := meta.EmbedFailed
	leanNoEmbedder := s.EmbedClient == nil
	if embedFailed && !leanNoEmbedder {
		out.Endpoint = s.EmbedClient.Endpoint()
	}
	// Probe index emptiness only when both lanes whiffed — keeps the EXISTS
	// query off the hot path, and lets a 0-chunk index (e.g. no index.include)
	// be reported as a config problem rather than a no-match (#161).
	if len(out.Symbols) == 0 && len(out.SemanticHits) == 0 {
		indexEmpty := false
		if empty, err := st.IsEmpty(ctx); err == nil {
			indexEmpty = empty
		}
		if noLaneHits(embedFailed, leanNoEmbedder, indexEmpty, &out) {
			return nil, out, nil
		}
	}

	// Near-miss surface for symbol_lookup whiffs — only when the symbol lane
	// actually whiffed. symbolNearMiss does a substring scan, so without this
	// gate a query that has exact defs but is also a substring of other names
	// (Run ⊂ runBench/RunResult) would emit a contradictory "no exact symbol
	// match" hint alongside the exact symbols[] it found (#533).
	if len(out.Symbols) == 0 {
		if hint := symbolNearMiss(ctx, st, intent, candidates); hint != "" {
			out.Hint = hint
		}
	}
	out.Status = "ok"
	if embedFailed && out.Hint == "" {
		out.Hint = "embed offline; results from symbol lane only."
	}
	s.activityRecord(p.Root, 1)
	if out.Hint == "" {
		out.Hint = s.activityNudge(p.Root, out.SessionTask)
	}
	// Task-start packs carry the local rules that govern the working set, so the
	// agent sees the constraints before editing. Folded in from brief (#141);
	// assemble is the "starting a task" intent, so only it pays this cost.
	if intent == retrieve.IntentAssemble {
		out.Rules = collectLocalRules(p.Root)
	}
	// Synthesize a grounded prose answer from the evidence just
	// assembled. Best-effort: a missing/unreachable chat client leaves
	// out.Answer empty and the agent falls back to the evidence bundle.
	logTok := resolveLogSink(ctx, tokenSink, req)
	// Loop detection: check after evidence is assembled so a block still
	// fires even when the question changes but the search pattern repeats.
	if blocked := s.applyLoopThrottle(in.Question, &out); blocked {
		return nil, out, nil
	}

	s.maybeAnswerStyle(ctx, logTok, intent, in, &out)

	// Stamp expansion handles on every locator the bundle hands back (#344),
	// after truncation so dropped hits don't get handles.
	stampSemHandles(out.SemanticHits)
	stampSymbolHandles(out.Symbols)
	stampReadHandles(out.SuggestedReads)

	// Cross-turn dedup (#344): mark locators already surfaced to this session on
	// an earlier turn and drop their content, so we don't resend bytes the agent
	// already holds.
	s.applySeenContext(sessionKey(req), &out)

	// Final safety net: keep the whole serialized bundle under a hard ceiling.
	// The inline byte pool bounds suggested_reads + semantic_hits, but graph and
	// knowledge_facts are appended outside it, so a dense graph on top of a full
	// exploration pool could still overflow the MCP tool-result limit (#784).
	clampResponseEnvelope(&out, intent)
	return nil, out, nil
}

// envelopeCeilingBytes is the hard cap on one ask response's serialized size,
// derived from the intent's inline pool budget plus headroom for the lanes that
// sit outside the pool (graph, knowledge_facts, annotations, answer). It stays
// well under the MCP tool-result token ceiling — a ~62 KB bundle has overflowed
// it in practice (#784).
func envelopeCeilingBytes(intent string) int {
	const outOfPoolHeadroom = 10 * 1024
	return retrieve.InlineCapsFor(intent).TotalBytesCap + outOfPoolHeadroom
}

// clampResponseEnvelope trims the bundle to fit envelopeCeilingBytes, shedding
// the lowest-value lanes first. The graph is a structural hint the agent can
// rebuild with trace(), so it goes before any inlined code: edges first (the
// bulk), then the nodes. If that is still not enough, inlined Content is
// dropped from the lowest-ranked semantic hits — tail first, never the top
// hit — leaving their locators so the agent can Read them. A no-op in the
// common case, where the bundle is already under budget.
func clampResponseEnvelope(out *ContextOutput, intent string) {
	ceiling := envelopeCeilingBytes(intent)
	if envelopeSizeBytes(out) <= ceiling {
		return
	}
	trimmed := false
	if out.Graph != nil && len(out.Graph.Edges) > 0 {
		out.Graph.Edges = nil
		trimmed = true
	}
	if out.Graph != nil && envelopeSizeBytes(out) > ceiling {
		out.Graph = nil
		trimmed = true
	}
	for envelopeSizeBytes(out) > ceiling {
		i := lastInlinedSemHit(out.SemanticHits)
		if i < 0 {
			break // nothing left to shed but the top hit
		}
		out.SemanticHits[i].Content = ""
		out.SemanticHits[i].Truncated = true
		trimmed = true
	}
	if trimmed {
		out.NextAction = strings.TrimSpace(out.NextAction +
			" [dex] Response trimmed to fit the size budget — graph and/or inlined tails dropped; trace() or Read the locators for the rest.")
	}
}

// envelopeSizeBytes is the serialized length the caller will receive. A marshal
// error (which should not happen for this struct) returns 0 so the clamp is a
// no-op rather than trimming a bundle it cannot measure.
func envelopeSizeBytes(out *ContextOutput) int {
	b, err := json.Marshal(out)
	if err != nil {
		return 0
	}
	return len(b)
}

// lastInlinedSemHit returns the index of the lowest-ranked semantic hit that
// still carries inlined Content, or -1 when only the top hit (index 0) does.
// Trimming tail-first preserves the highest-scoring evidence.
func lastInlinedSemHit(hits []SemHit) int {
	for i := len(hits) - 1; i >= 1; i-- {
		if hits[i].Content != "" {
			return i
		}
	}
	return -1
}

// resolveLogSink returns the token sink to use for answer streaming.
// An explicit sink (CLI path) wins; for MCP sessions the sink wraps
// session.Log so tokens arrive as Log notifications; otherwise nil.
func resolveLogSink(ctx context.Context, tokenSink func(string), req *sdk.CallToolRequest) func(string) {
	if tokenSink != nil {
		return tokenSink
	}
	if req == nil || req.Session == nil {
		return nil
	}
	session := req.Session
	return func(tok string) {
		_ = session.Log(ctx, &sdk.LoggingMessageParams{
			Level:  "debug",
			Logger: "dex/ask",
			Data:   tok,
		})
	}
}

// noLaneHits sets out.Status/Hint and returns true when both retrieval lanes
// returned nothing. The caller should return immediately on true.
func noLaneHits(embedFailed, leanNoEmbedder, indexEmpty bool, out *ContextOutput) bool {
	if len(out.Symbols) > 0 || len(out.SemanticHits) > 0 {
		return false
	}
	// An empty index is the dominant, retry-proof cause — no query and no
	// embedder state can conjure a match from 0 chunks. Report it ahead of the
	// embed-failed / no-match branches so the agent fixes the config, not the
	// phrasing (#161).
	if indexEmpty {
		out.Status = "index-empty"
		out.Hint = "index is empty (0 chunks) — likely no index.include in .dex/config.yml; run `dex doctor` for the diagnosis, then `dex index`."
		out.NextAction = "This repo's index has 0 chunks, so no query can match. Add an index.include allow-list (see dex doctor), re-run dex index, then retry — do not rephrase."
		return true
	}
	if embedFailed {
		if leanNoEmbedder {
			out.Status = "lean-no-embedder"
			out.Hint = "lean profile (DEX_EMBED_ENGINE=none): no semantic lane — use lookup, grep, or the trace/impact graph tools."
		} else {
			out.Status = "embedding-service-unreachable"
			out.Hint = "the local embedding service is offline — fall back to grep / Glob / ripgrep for this query."
		}
		return true
	}
	out.Status = "ok"
	out.Hint = "no matches; try broader phrasing or a more specific identifier."
	out.NextAction = "Try rephrasing the question with concrete keywords from the codebase, or fall back to grep."
	return true
}

// symbolNearMiss returns a hint string when the question is a symbol_lookup
// with no exact hits. It scans chunks for substring candidates so the agent
// gets names without a follow-up tool call.
func symbolNearMiss(ctx context.Context, st store.Searcher, intent string, candidates retrieve.IntentCandidates) string {
	if intent != retrieve.IntentSymbolLookup || len(candidates.Identifiers) == 0 {
		return ""
	}
	var cands []string
	for _, id := range candidates.Identifiers {
		bare := id
		if i := strings.LastIndex(bare, "."); i >= 0 {
			bare = bare[i+1:]
		}
		names, err := st.FindSymbolCandidates(ctx, bare, 5)
		if err != nil {
			continue
		}
		cands = append(cands, names...)
		if len(cands) >= 5 {
			cands = cands[:5]
			break
		}
	}
	if len(cands) == 0 {
		return ""
	}
	return "no exact symbol match — did you mean: " + strings.Join(cands, ", ") + "?"
}

// applyLoopThrottle applies loop-detection throttling. It returns true when
// the call is blocked (caller should return early with out unchanged).
func (s *Server) applyLoopThrottle(question string, out *ContextOutput) bool {
	ldLevel, ldHint := s.ld().Check("ask", throttle.ArgsKey(question), true)
	if ldLevel == throttle.Block {
		out.Status = "loop-blocked"
		out.Hint = ldHint
		out.SemanticHits = nil
		out.Symbols = nil
		return true
	}
	if ldLevel == throttle.Reduce {
		if len(out.SemanticHits) > 3 {
			out.SemanticHits = out.SemanticHits[:3]
		}
		if len(out.Symbols) > 3 {
			out.Symbols = out.Symbols[:3]
		}
		out.Hint = ldHint + " [reduced]"
	} else if ldHint != "" && out.Hint == "" {
		out.Hint = ldHint
	}
	return false
}
