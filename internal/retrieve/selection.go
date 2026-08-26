package retrieve

// Selection is the uniform result currency every lane emits and every pipe
// stage (#206) consumes. It is the universalization of the #95 domain/wire seam:
// ContextPack gave the semantic/assemble lane a domain result with a folded
// Trust envelope, but only 1 of 3 lane families had one. Selection generalizes
// that seam to every lane — a thin set of located Refs plus the trust envelope,
// the stages that ran, and the remaining budget. See
// docs/design/95f-selection-spine.md.
//
// Like ContextPack it is transport-free (no json tags): mcp projects it to a
// wire twin at L4. ContextPack embeds Selection rather than being replaced by it
// (#95f §3) — the rich typed evidence lanes stay; Refs is a lightweight flat
// index OVER that evidence, the handle pipe stages thread.
type Selection struct {
	Refs   []Ref    // located entities, the currency pipe stages thread
	Trust  Trust    // folded freshness/confidence envelope (#95c), one home
	Stages []string // lane segments run, echoed to route.stages; length 1 today
	Budget int      // remaining token budget; each pipe stage debits (#206)
}

// Ref is one located code entity flowing through a lane or a pipe stage — the
// atom of a Selection. It is a locator, not a payload: terminals render the
// bytes, transforms walk the graph; Meta carries lane-specific context so a
// terminal need not re-fetch.
type Ref struct {
	Kind  string         // file | symbol | chunk | package | edge
	ID    string         // stable id: path, qualified symbol, node id
	Path  string         // rel path when applicable
	Span  [2]int         // [startLine, endLine] when applicable
	Prov  string         // exact | semantic | name-based | model — provenance
	Score float64        // rank/relevance within its lane
	Meta  map[string]any // lane-specific extras (edge kind, matched line, …)
}
