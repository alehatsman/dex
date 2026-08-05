package retrieve

import "time"

// ContextPack is the assembled, intent-shaped working set for one ask — the
// L2 DOMAIN result of assembly. It carries no wire (json) tags: mcp projects it
// to ContextOutput at the surface (L4). Splitting the domain pack out of the
// wire type lets the assembly logic live where it belongs and gives the trust
// envelope a native home next to the facts. See docs/design/95-context-pack.md.
//
// This is the target type the #95a seam-move lands on; no caller builds a
// ContextPack yet (#101 / #95b defines the shape only).
type ContextPack struct {
	Intent   string // resolved intent (ResolveIntent)
	Question string

	// --- Evidence lanes ---
	Symbols        []SymbolHit     // domain SymbolHit (twin of mcp's wire type)
	SemanticHits   []SemHit        // existing retrieve.SemHit
	SuggestedReads []SuggestedRead // existing retrieve.SuggestedRead
	Graph          *GraphResult    // existing retrieve.GraphResult
	References     []RefHit
	Annotations    map[string]PathMeta // per-file enrichment keyed by rel path
	RelatedFiles   []string            // spreading activation (#688), assemble intent only

	// --- Accumulated knowledge ---
	KnowledgeFacts []string     // top project facts by salience
	ScopedNotes    []ScopedNote // notes bound to a touched file's path (#645)

	// --- Completeness (#725) ---
	Concerns Concerns

	// --- Prose directives (assembled from the evidence above) ---
	NextAction string // what to do next given the bundle (#725/#729)
	Avoid      string // anti-pattern warning for this intent

	// --- Trust envelope (#95c) ---
	Trust Trust

	// --- Cost accounting ---
	ContentBytesInlined int
	Expanded            bool // query expansion contributed terms (#252)
}

// Trust folds the confidence/freshness signals dex already computes but
// currently scatters (ContextOutput.Stale/Indexing, SemHit.Lanes,
// LowConfidenceScore, graphquery.EdgeKind, the recall caveat). #101 defines the
// shape; #95c populates it and surfaces the two genuinely new fields
// (LowConf, GraphResolved/RecallPartial) that are computed internally today and
// dropped before the response.
type Trust struct {
	// Freshness — is the index behind the working tree?
	Stale     bool      // index older than the working tree
	Indexing  bool      // a reindex is underway; results are partial (#531)
	IndexedAt time.Time // index mtime; drives age

	// Confidence — how much to trust the ranking.
	TopScore   float32 // fused top semantic score
	LowConf    bool    // TopScore < LowConfidenceScore (0.45)
	Confidence string  // "high" | "medium" | "low" self-assessment (ConfidenceLevel)

	// Claims — proven graph facts vs heuristic edges.
	GraphResolved bool   // all graph edges type-resolved (Go)
	RecallPartial bool   // name-based edges present → recall incomplete
	Caveat        string // human-readable recall warning (response.go)
}

// Concerns is the assemble completeness signal (#725): which query concerns the
// inlined working set is ABOUT (Covered) vs which the byte budget dropped
// (Dropped). An honest partial beats a false floor.
type Concerns struct {
	Covered []string
	Dropped []string
}

// SymbolHit is the one neutral (transport-free) symbol type used across the
// retrieve package — the symbol-lane row, the pack member, and the shape the
// prose/inline/graph lanes read (#112). It maps to the wire mcp.SymbolHit at
// L4 for the presentation fields; the lane-input fields below (Name +
// centrality) have no wire twin — they exist only to feed the injected Role
// formatter and are zero once Role is set.
//
// Field lifecycle: SymbolLane fills the identity + centrality columns; the
// assembler's injected FormatRole reads Name/centrality to fill Role; enrich
// fills Signature/Doc; inline fills Body/Truncated; envelope fills Handle/SeenTurn.
type SymbolHit struct {
	QualifiedName string
	// Name is the raw symbol name as stored (may be empty); FormatRole's first
	// argument. Lane input — not projected to the wire type.
	Name      string
	Path      string
	StartLine int
	EndLine   int
	Kind      string
	// Call-graph centrality columns — inputs the injected FormatRole consumes
	// to render Role. Lane inputs; unread by the prose/inline/graph lanes and
	// not projected to the wire type.
	InDegree        int
	OutDegree       int
	CrossPkgCallers int
	Betweenness     float64
	Signature       string // declaration line — the API contract without the body
	Doc             string // contiguous comment block above the decl
	Body            string // full source, populated only for symbol_lookup intent
	Role            string
	Truncated       bool
	Handle          string // opaque expansion handle for this range (#344)
	SeenTurn        int    // >0 when already surfaced this session (#344)
}

// RefHit is a single BM25-backed reference to a symbol (domain twin of
// mcp.RefHit). Stand-in for the deferred `calls` graph edges.
type RefHit struct {
	Path    string
	Line    int
	Snippet string // single-line excerpt
	Symbol  string // which symbol this is a ref to
}

// PathMeta is the per-file annotation bundle keyed by relative path in
// ContextPack.Annotations (domain twin of mcp.PathMeta). Fields are populated
// conditionally by intent and may individually be empty — all data about a
// single file lives in one place, joined by path.
type PathMeta struct {
	LastCommit string   // short SHA + short date + author (editing_context)
	LastAuthor string   // author of the last commit (editing_context)
	Owners     []string // CODEOWNERS matches (editing_context)
	NearestDoc string   // closest doc walking up: CLAUDE.md > doc.go > README.md
	Tests      []string // sibling test files paired by language convention
	BuildTags  string   // //go:build constraint + package clause
	Package    string   // package name (Go)
}

// ScopedNote is a durable note bound to a file's path via its scope
// (gotcha-on-touch, #645) — the domain twin of mcp.LocatedFact.
type ScopedNote struct {
	ID        int64
	Archetype string
	Body      string
	Salience  float64
	Scope     string
}
