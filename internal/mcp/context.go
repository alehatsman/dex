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
// internal/graph (Go-only). Other languages still get a ripgrep-
// backed `references` list as a fallback.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/alehatsman/dex/internal/retrieve"
	"github.com/alehatsman/dex/internal/store"
	"github.com/alehatsman/dex/internal/throttle"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ─── tool: ask ────────────────────────────────────────────────────────────

type ContextInput struct {
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
	Question    string `json:"question" jsonschema:"free-text question about the codebase (e.g. 'where is filesystem event debouncing handled?', 'how does indexing work?', 'callers of (*Store).Search')"`
	Intent      string `json:"intent,omitempty" jsonschema:"force a strategy: auto|behavior_search|symbol_lookup|callers|callees|architecture|package_topology|editing_context|assemble (default: auto). assemble returns a budget-bounded working set — symbol bodies chosen by submodular keyword coverage, prose synthesis suppressed — instead of a prose answer (#687)"`
	K           int    `json:"k,omitempty" jsonschema:"max hits per lane (default 8, max 30)"`
	NoInline    bool   `json:"no_inline,omitempty" jsonschema:"skip inlining file contents into suggested_reads and semantic_hits. Default off: both lanes carry their line-range content from one shared per-intent byte pool (per-range cap ~60 lines / 4 KB; total cap ~20 KB targeted / ~40 KB exploration; oversize ranges are clipped with truncated=true). Set true if you already have the files open, or in long sessions where context budget is limited — check content_bytes_inlined from a prior ask to gauge how much was consumed."`
	Expand      string `json:"expand,omitempty" jsonschema:"opt-in query-side expansion (#252): off|on|full. on adds model-generated keywords+identifiers to the BM25 and symbol lanes (no extra embedding); full also embeds a hypothetical-answer passage into the vector lane. Empty defers to the server default (DEX_EXPAND_MODE). Requires DEX_EXPAND_MODEL to be configured; otherwise a no-op."`
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
	Content   string  `json:"content,omitempty"`
	Truncated bool    `json:"truncated,omitempty"`
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
	Status string `json:"status"` // ok | no-index | embedding-service-unreachable | error
	Hint   string `json:"hint,omitempty"`
	// Answer is the synthesized prose response to the question, grounded
	// in the evidence below and citing `path:line`. Populated when a chat
	// client is configured and reachable; absent on degradation, in which
	// case the agent works from the evidence bundle + next_action.
	Answer string `json:"answer,omitempty"`
	// AnswerModel names the model that produced Answer (e.g.
	// "qwen2.5-coder:14b"). Empty when no answer was synthesized.
	AnswerModel    string          `json:"answer_model,omitempty"`
	Endpoint       string          `json:"endpoint,omitempty"` // populated when embed is unreachable
	Project        string          `json:"project,omitempty"`
	Intent         string          `json:"intent,omitempty"`
	Stale          bool            `json:"stale,omitempty"`
	Indexing       bool            `json:"indexing,omitempty"` // a re-index is underway; results are partial (#531)
	SemanticHits   []SemHit        `json:"semantic_hits,omitempty"`
	Symbols        []SymbolHit     `json:"symbols,omitempty"`
	Graph          *GraphResult    `json:"graph,omitempty"`
	SuggestedReads []SuggestedRead `json:"suggested_reads,omitempty"`
	NextAction     string          `json:"next_action,omitempty"`
	Avoid          string          `json:"avoid,omitempty"`
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

func (s *Server) contextRouterStream(ctx context.Context, req *sdk.CallToolRequest, in ContextInput, tokenSink func(string)) (*sdk.CallToolResult, ContextOutput, error) {
	if strings.TrimSpace(in.Question) == "" {
		// Empty question = session-start orientation: return the deterministic
		// L0+L1 map so the agent names the right cluster before any find()
		// (#348 / #316 story 6). No inference, byte-stable, cache-friendly.
		return s.orientResponse(ctx, in)
	}
	p, hint := s.resolveProject(in.ProjectRoot)
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

	if stats, statsErr := st.Stats(ctx); statsErr == nil && !stats.LastIndex.IsZero() && time.Since(stats.LastIndex) > 24*time.Hour {
		out.Stale = true
		out.Hint = fmt.Sprintf("index is %s old — run `dex index %s` to refresh.",
			time.Since(stats.LastIndex).Round(time.Hour), p.Root)
	}
	// An active rebuild trumps age: evidence is being rewritten right now, so
	// what we return is partial (#531).
	if indexing, note := indexingNotice(ctx, st); indexing {
		out.Stale = true
		out.Indexing = true
		out.Hint = note
	}

	if ss, ok, err := st.SessionGet(ctx); err == nil && ok && ss.Task != "" {
		out.SessionTask = ss.Task
	}
	if facts, err := s.recallFacts(ctx, st, in.Question, 5, true); err == nil && len(facts) > 0 {
		for _, f := range facts {
			out.KnowledgeFacts = append(out.KnowledgeFacts, "["+f.Archetype+"] "+f.Body)
		}
	}

	// enrichGraph sets out.Graph only when it has something to emit.
	// An absent `graph` key signals "no graph indexed, or this intent
	// surfaced no structural context" — saves bytes over shipping
	// `{nodes:[], edges:[]}` on every response.

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

	// Symbol lane — exact identifier lookups. Cheap, no embed required.
	// Runs whenever the question contains identifier-shaped tokens, even
	// for non-symbol intents (a behavior_search question that mentions
	// `(*Store).Search` benefits from the structural lane too).
	symbols, symbolPaths := s.runSymbolLane(ctx, st, candidates, k)
	out.Symbols = symbols

	// Semantic lane — runs unless embed is offline. We always run it
	// for recall even when the symbol lane has exact hits. The embed text
	// stays the raw question (no extra GPU) unless full-mode HyDE is on;
	// the BM25 text carries the expansion keywords/identifiers for free.
	semHits, embedFailed := s.runSemanticLane(ctx, st, retrieve.ExpandedEmbedText(in.Question, exp), retrieve.ExpandedFTSText(in.Question, exp), k)
	leanNoEmbedder := s.EmbedClient == nil
	if embedFailed && !leanNoEmbedder {
		out.Endpoint = s.EmbedClient.Endpoint()
	}
	out.SemanticHits = semHits

	if noLaneHits(embedFailed, leanNoEmbedder, &out) {
		return nil, out, nil
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

	enrichGraph(&out, intent, graphView, out.SemanticHits, out.Symbols)
	out.SuggestedReads = pickSuggestedReads(intent, out.SemanticHits, out.Symbols, symbolPaths, graphView)
	if !in.NoInline {
		inlineContent(p.Root, intent, out.SuggestedReads, out.Symbols, out.SemanticHits, candidates.Identifiers)
		out.ContentBytesInlined = countInlinedBytes(out.SuggestedReads, out.Symbols, out.SemanticHits)
	}
	(&Enricher{projectRoot: p.Root, Store: st}).Enrich(ctx, intent, k, &out)
	topSem := maxSemanticScore(out.SemanticHits)
	var graphEdgeCount int
	if out.Graph != nil {
		graphEdgeCount = len(out.Graph.Edges)
	}
	out.NextAction = buildNextAction(intent, out.SuggestedReads, out.Symbols, topSem,
		graphEdgeCount, len(out.References), hasBlameAnnotations(out.Annotations))
	// If the directive's primary read was truncated at inline time,
	// flag that so the agent knows the inlined Content isn't the full
	// chunk and can Read the original line range for the rest.
	if !in.NoInline && len(out.SuggestedReads) > 0 && out.SuggestedReads[0].Truncated {
		out.NextAction += " The inlined content is truncated at inline-budget caps — Read the full line range if you need the tail."
	}
	out.Avoid = buildAvoid(intent, out.SemanticHits, out.Symbols, graphView != nil, len(out.References) > 0)
	out.Status = "ok"
	if embedFailed && out.Hint == "" {
		out.Hint = "embed offline; results from symbol lane only."
	}
	s.activityRecord(p.Root, 1)
	if out.Hint == "" {
		out.Hint = s.activityNudge(p.Root, out.SessionTask)
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

	// synthesizeAnswer self-skips the assemble intent (#687): assemble returns the
	// structured working set, not prose, so the bundle IS the answer.
	s.synthesizeAnswer(ctx, logTok, intent, in.Question, &out)

	// next_action was built deterministically from suggested_reads[0] before
	// the answer existed; realign it so it never points away from the file the
	// answer leads with (#532).
	reconcileNextActionWithAnswer(&out)

	// Stamp expansion handles on every locator the bundle hands back (#344),
	// after truncation so dropped hits don't get handles.
	stampSemHandles(out.SemanticHits)
	stampSymbolHandles(out.Symbols)
	stampReadHandles(out.SuggestedReads)

	// Cross-turn dedup (#344): mark locators already surfaced to this session on
	// an earlier turn and drop their content, so we don't resend bytes the agent
	// already holds.
	s.applySeenContext(sessionKey(req), &out)
	return nil, out, nil
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
func noLaneHits(embedFailed, leanNoEmbedder bool, out *ContextOutput) bool {
	if len(out.Symbols) > 0 || len(out.SemanticHits) > 0 {
		return false
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
