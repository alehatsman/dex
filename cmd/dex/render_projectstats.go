// Output rendering for the CLI subcommands. Splitting these out keeps
// main.go focused on dispatch + env wiring, and makes it obvious which
// pieces are "presentation only" vs "real work".
package main

import (
	"fmt"
	"time"
)

// projectStats is the set of fields the project status renderer
// needs. Decoupled from store.Stats so the formatter doesn't depend
// on the internal package layout.
type projectStats struct {
	lastIndex time.Time
	files     int
	chunks    int
	nodes     int64
	edges     int64
	dim       int  // optional — emitted only when > 0
	stale     bool // embed model mismatch — reindex required
}

// printProjectStatLines emits the labelled key:value rows that
// describe one project, all sharing the given indent prefix. Labels
// are padded to a fixed width so values line up vertically inside
// the block. Optional rows (graph, summaries, dim) are skipped when
// there's nothing to show — better than emitting "graph: 0 nodes 0
// edges" which would just be noise.
func printProjectStatLines(indent string, st projectStats) {
	const labelWidth = 8 // "indexed:" is the longest label
	field := func(label, value string) {
		fmt.Printf("%s%-*s %s\n", indent, labelWidth, label+":", value)
	}
	field("indexed", formatProjectAge(st.lastIndex))
	field("files", fmt.Sprintf("%d", st.files))
	field("chunks", fmt.Sprintf("%d", st.chunks))
	if st.nodes > 0 || st.edges > 0 {
		field("graph", fmt.Sprintf("%d nodes  %d edges", st.nodes, st.edges))
	}
	if st.stale {
		field("embed", "changed — reindex required")
	}
	if st.dim > 0 {
		field("dim", fmt.Sprintf("%d", st.dim))
	}
}

// formatProjectAge renders the indexed-time string for one project.
// Zero timestamps return "—". Stale entries (>24h since last index)
// carry an explicit " stale" suffix instead of relying on the ⚠
// symbol — easier to scan, no glyph dependency.
func formatProjectAge(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	rel := relativeTime(t)
	if time.Since(t) > 24*time.Hour {
		return rel + "  stale"
	}
	return rel
}
