package graphquery

import "github.com/alehatsman/dex/internal/graph"

// BuildCycles computes Tarjan SCCs over the `calls` edges in the view
// and returns IDs of components of size ≥ minSize, sorted by descending
// size. Pure over view — unit-testable.
func BuildCycles(view *View, minSize int) [][]string {
	nodes := make([]graph.Node, 0, len(view.NodesByID))
	for _, n := range view.NodesByID {
		nodes = append(nodes, graph.Node{
			ID:          n.ID,
			PackagePath: n.PackagePath,
		})
	}
	edges := make([]graph.Edge, 0, len(view.EdgesByKind[graph.EdgeCalls]))
	for _, e := range view.EdgesByKind[graph.EdgeCalls] {
		edges = append(edges, graph.Edge{
			Kind:  graph.EdgeCalls,
			SrcID: e.SrcID,
			DstID: e.DstID,
		})
	}
	sccs := graph.TarjanSCC(nodes, edges, nil)
	var out [][]string
	for _, scc := range sccs {
		if len(scc.IDs) >= minSize {
			out = append(out, scc.IDs)
		}
	}
	return out
}
