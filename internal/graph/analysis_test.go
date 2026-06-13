package graph

import (
	"testing"
)

func nodesOf(ids ...string) []Node {
	out := make([]Node, len(ids))
	for i, id := range ids {
		out[i] = Node{ID: id, Kind: NodeFunction, PackagePath: "pkg"}
	}
	return out
}

func callEdges(pairs ...string) []Edge {
	out := make([]Edge, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, Edge{Kind: EdgeCalls, SrcID: pairs[i], DstID: pairs[i+1]})
	}
	return out
}

func sccIDs(sccs []SCCResult) [][]string {
	out := make([][]string, len(sccs))
	for i, s := range sccs {
		cp := make([]string, len(s.IDs))
		copy(cp, s.IDs)
		out[i] = cp
	}
	return out
}

// TestTarjanSCCCycle: A→B→C→A is one SCC of size 3.
func TestTarjanSCCCycle(t *testing.T) {
	nodes := nodesOf("A", "B", "C")
	edges := callEdges("A", "B", "B", "C", "C", "A")
	sccs := TarjanSCC(nodes, edges, nil)
	if len(sccs) != 1 {
		t.Fatalf("expected 1 SCC, got %d: %v", len(sccs), sccIDs(sccs))
	}
	if len(sccs[0].IDs) != 3 {
		t.Errorf("SCC size = %d, want 3", len(sccs[0].IDs))
	}
}

// TestTarjanSCCAcyclic: A→B→C forms a DAG — each node is its own trivial SCC.
func TestTarjanSCCAcyclic(t *testing.T) {
	nodes := nodesOf("A", "B", "C")
	edges := callEdges("A", "B", "B", "C")
	sccs := TarjanSCC(nodes, edges, nil)
	if len(sccs) != 3 {
		t.Fatalf("expected 3 trivial SCCs, got %d", len(sccs))
	}
	for _, s := range sccs {
		if len(s.IDs) != 1 {
			t.Errorf("unexpected non-trivial SCC: %v", s.IDs)
		}
	}
}

// TestTarjanSCCSelfLoop: A→A is a cycle of size 1.
func TestTarjanSCCSelfLoop(t *testing.T) {
	nodes := nodesOf("A")
	edges := callEdges("A", "A")
	sccs := TarjanSCC(nodes, edges, nil)
	if len(sccs) != 1 {
		t.Fatalf("got %d SCCs, want 1", len(sccs))
	}
	if len(sccs[0].IDs) != 1 || sccs[0].IDs[0] != "A" {
		t.Errorf("unexpected SCC: %v", sccs[0].IDs)
	}
}

// TestTarjanSCCDisconnected: isolated nodes each form their own trivial SCC.
func TestTarjanSCCDisconnected(t *testing.T) {
	nodes := nodesOf("A", "B", "C")
	sccs := TarjanSCC(nodes, nil, nil)
	if len(sccs) != 3 {
		t.Fatalf("got %d SCCs for disconnected graph, want 3", len(sccs))
	}
}

// TestTarjanSCCTwoCycles: two independent cycles, returned largest-first.
func TestTarjanSCCTwoCycles(t *testing.T) {
	nodes := nodesOf("A", "B", "C", "D", "E")
	// cycle A↔B (size 2) + cycle C→D→E→C (size 3)
	edges := callEdges("A", "B", "B", "A", "C", "D", "D", "E", "E", "C")
	sccs := TarjanSCC(nodes, edges, nil)
	if len(sccs) != 2 {
		t.Fatalf("got %d SCCs, want 2", len(sccs))
	}
	if len(sccs[0].IDs) < len(sccs[1].IDs) {
		t.Errorf("SCCs not sorted by descending size: %d < %d", len(sccs[0].IDs), len(sccs[1].IDs))
	}
	if len(sccs[0].IDs) != 3 {
		t.Errorf("largest SCC size = %d, want 3", len(sccs[0].IDs))
	}
}

// TestTarjanSCCIgnoresNonCallEdges: HasMethod edges are not traversed.
func TestTarjanSCCIgnoresNonCallEdges(t *testing.T) {
	nodes := nodesOf("A", "B")
	edges := []Edge{
		{Kind: EdgeHasMethod, SrcID: "A", DstID: "B"},
		{Kind: EdgeHasMethod, SrcID: "B", DstID: "A"},
	}
	sccs := TarjanSCC(nodes, edges, nil)
	// HasMethod edges are not in the default edgeKinds (EdgeCalls only).
	for _, s := range sccs {
		if len(s.IDs) > 1 {
			t.Errorf("non-call edge formed an SCC: %v", s.IDs)
		}
	}
}

// TestTarjanSCCCustomEdgeKind: custom edgeKinds parameter.
func TestTarjanSCCCustomEdgeKind(t *testing.T) {
	nodes := nodesOf("A", "B")
	edges := []Edge{
		{Kind: EdgeHasMethod, SrcID: "A", DstID: "B"},
		{Kind: EdgeHasMethod, SrcID: "B", DstID: "A"},
	}
	sccs := TarjanSCC(nodes, edges, []EdgeKind{EdgeHasMethod})
	if len(sccs) != 1 || len(sccs[0].IDs) != 2 {
		t.Errorf("expected 1 SCC of size 2 with custom edge kind, got %v", sccIDs(sccs))
	}
}

// TestTarjanSCCEmpty: empty input returns empty result.
func TestTarjanSCCEmpty(t *testing.T) {
	sccs := TarjanSCC(nil, nil, nil)
	if len(sccs) != 0 {
		t.Errorf("got %d SCCs for empty input, want 0", len(sccs))
	}
}

// TestPageRankBasic: hub node (in-degree 2) should outrank leaf nodes.
func TestPageRankBasic(t *testing.T) {
	// A→C, B→C — C is the hub
	outAdj := map[string]map[string]struct{}{
		"A": {"C": {}},
		"B": {"C": {}},
		"C": {},
	}
	pr := PageRank([]string{"A", "B", "C"}, outAdj)
	if pr["C"] <= pr["A"] || pr["C"] <= pr["B"] {
		t.Errorf("hub C should outrank A and B: A=%v B=%v C=%v", pr["A"], pr["B"], pr["C"])
	}
}

// TestPageRankSumToOne: scores should sum to ~1.0 (probability mass conservation).
func TestPageRankSumToOne(t *testing.T) {
	outAdj := map[string]map[string]struct{}{
		"A": {"B": {}},
		"B": {"C": {}},
		"C": {"A": {}},
	}
	pr := PageRank([]string{"A", "B", "C"}, outAdj)
	var sum float64
	for _, v := range pr {
		sum += v
	}
	const eps = 1e-4
	if sum < 1-eps || sum > 1+eps {
		t.Errorf("PageRank sum = %v, want ~1.0", sum)
	}
}

// TestPageRankEmpty: empty input returns empty map without panic.
func TestPageRankEmpty(t *testing.T) {
	pr := PageRank(nil, nil)
	if len(pr) != 0 {
		t.Errorf("got %d entries, want 0", len(pr))
	}
}
