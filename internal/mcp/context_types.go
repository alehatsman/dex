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
