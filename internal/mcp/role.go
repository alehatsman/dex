package mcp

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/alehatsman/dex/internal/feedback"
)

// formatRole composes the compact role tag attached to symbol-shaped
// results (SearchHit.Role, CallSite.Role). The tag is meant to give an
// agent a 1-glance read on how the symbol sits in the call graph —
// without it having to call graph_callers or graph_callees first.
//
// Rules, in priority order:
//
//	"central:<in>/<pkg>pkg"  — in_degree ≥ 5 OR cross_pkg_callers ≥ 2.
//	                           The headline tier: lots of callers, or
//	                           callers spanning real package boundaries.
//	                           The pkg suffix is omitted when 0.
//	"bridge:<bw>%"           — betweenness ≥ 0.1 AND not already
//	                           "central". A bridge node sits on many
//	                           shortest call-paths; removing it most
//	                           disconnects the graph.
//	"exported-unused"        — name begins with an upper-case rune
//	                           (Go's exportedness rule) AND in_degree
//	                           == 0. Useful for spotting dead public
//	                           API and APIs only consumed externally.
//	"leaf"                   — out_degree == 0 AND in_degree > 0.
//	                           The symbol is called but calls nothing
//	                           indexed itself — typically a base-case
//	                           helper or an io/syscall wrapper.
//	""                       — unremarkable middle (the common case).
//	                           Also the result when no graph node
//	                           exists (all-zero centrality).
//
// Thresholds chosen empirically against this repo: in_degree=5 cleanly
// separates utility helpers from real domain symbols, and pkg=2 catches
// genuine cross-package APIs without flagging every type that happens
// to be referenced from one neighbour.
func formatRole(name string, inDegree, outDegree, crossPkg int, betweenness float64) string {
	allZero := inDegree == 0 && outDegree == 0 && crossPkg == 0 && betweenness == 0
	if allZero {
		return ""
	}
	if inDegree >= 5 || crossPkg >= 2 {
		if crossPkg > 0 {
			return fmt.Sprintf("central:%d/%dpkg", inDegree, crossPkg)
		}
		return fmt.Sprintf("central:%d", inDegree)
	}
	if betweenness >= 0.1 {
		return fmt.Sprintf("bridge:%d%%", int(betweenness*100))
	}
	if inDegree == 0 && isExported(name) {
		return "exported-unused"
	}
	if outDegree == 0 && inDegree > 0 {
		return "leaf"
	}
	return ""
}

// isExported is Go's exportedness check — first rune is upper-case.
// Used as a heuristic across languages here; for non-Go projects the
// convention often doesn't match, so "exported-unused" simply won't
// fire (centrality stays zero anyway when no graph is indexed).
func isExported(name string) bool {
	for _, r := range name {
		return unicode.IsUpper(r)
	}
	return false
}

// graphAgreement maps a formatRole tag to an agreement tier for the live
// feedback reweight (#220). Semantic hits carry a lane count (how many
// independent search lanes — vector/bm25/graph — surfaced the same hit) that
// feeds feedback.ShadowMultiplier; graph-lane hits (callers/callees) have no
// such per-hit signal, since they're single-sourced from the call graph. A
// node's centrality tier is the natural analogue: "central" means multiple
// independent signals (high in-degree, or callers spanning package
// boundaries) already agree the node matters, the same way multiple lanes
// agreeing does for a semantic hit. "bridge" is a weaker single-signal case.
// Anything else gets tier 1 — no boost, matching a single-lane semantic hit.
func graphAgreement(role string) int {
	switch {
	case strings.HasPrefix(role, "central"):
		return 3
	case strings.HasPrefix(role, "bridge"):
		return 2
	default:
		return 1
	}
}

// reweightedPageRank scales a peer's PageRank by the live feedback multiplier
// (#220), analogous to shadowReorder's per-hit rescoring for semantic hits.
// openRate/n ==(0, 0) (live reweight off, or no signal yet for this intent)
// degrades feedback.ShadowMultiplier to 1.0 — an identity scale, so callers
// don't need a separate off-path branch.
func reweightedPageRank(pageRank float64, role string, openRate float64, n int) float64 {
	return pageRank * feedback.ShadowMultiplier(openRate, n, graphAgreement(role))
}
