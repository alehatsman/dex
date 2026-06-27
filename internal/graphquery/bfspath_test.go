package graphquery

import (
	"testing"

	"github.com/alehatsman/dex/internal/graph"
)

// bfsView builds a small call graph for BFSPath tests:
//
//	A --calls--> B --calls--> C
//	A --calls--> D
func bfsView() *View {
	mkNode := func(name string) Node {
		return Node{
			ID:            name,
			Kind:          graph.NodeFunction,
			Name:          name,
			QualifiedName: "pkg." + name,
			PackagePath:   "pkg",
			FilePath:      "pkg/pkg.go",
			StartLine:     1,
		}
	}
	a := mkNode("A")
	b := mkNode("B")
	c := mkNode("C")
	d := mkNode("D")

	edge := func(src, dst Node) Edge {
		return Edge{Kind: graph.EdgeCalls, SrcID: src.ID, DstID: dst.ID}
	}
	edges := []Edge{edge(a, b), edge(b, c), edge(a, d)}

	v := &View{
		NodesByID:        map[string]Node{},
		NodesByName:      map[string][]Node{},
		NodesByQualified: map[string][]Node{},
		NodesByPackage:   map[string][]Node{},
		NodesByPath:      map[string][]Node{},
		EdgesBySrc:       map[string][]Edge{},
		EdgesByDst:       map[string][]Edge{},
		EdgesByKind:      map[graph.EdgeKind][]Edge{},
	}
	for _, n := range []Node{a, b, c, d} {
		v.NodesByID[n.ID] = n
		v.NodesByName[n.Name] = append(v.NodesByName[n.Name], n)
		v.NodesByQualified[n.QualifiedName] = append(v.NodesByQualified[n.QualifiedName], n)
		v.NodesByPath[n.FilePath] = append(v.NodesByPath[n.FilePath], n)
	}
	for _, e := range edges {
		v.EdgesBySrc[e.SrcID] = append(v.EdgesBySrc[e.SrcID], e)
		v.EdgesByDst[e.DstID] = append(v.EdgesByDst[e.DstID], e)
		v.EdgesByKind[e.Kind] = append(v.EdgesByKind[e.Kind], e)
	}
	return v
}

func TestBFSPath_DirectHop(t *testing.T) {
	v := bfsView()
	a := v.NodesByID["A"]
	hops := BFSPath(v, []Node{a}, map[string]bool{"D": true}, 5)
	if len(hops) != 2 {
		t.Fatalf("expected 2 hops (A→D), got %d", len(hops))
	}
	if hops[0].QualifiedName != "pkg.A" || hops[1].QualifiedName != "pkg.D" {
		t.Errorf("unexpected path: %v", hops)
	}
}

func TestBFSPath_MultiHop(t *testing.T) {
	v := bfsView()
	a := v.NodesByID["A"]
	hops := BFSPath(v, []Node{a}, map[string]bool{"C": true}, 5)
	if len(hops) != 3 {
		t.Fatalf("expected 3 hops (A→B→C), got %d: %v", len(hops), hops)
	}
	names := []string{hops[0].QualifiedName, hops[1].QualifiedName, hops[2].QualifiedName}
	want := []string{"pkg.A", "pkg.B", "pkg.C"}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("hop[%d]: got %q, want %q", i, names[i], want[i])
		}
	}
}

func TestBFSPath_NoPath(t *testing.T) {
	v := bfsView()
	// C has no outbound edges — no path from C to A.
	c := v.NodesByID["C"]
	hops := BFSPath(v, []Node{c}, map[string]bool{"A": true}, 5)
	if hops != nil {
		t.Errorf("expected nil path, got %v", hops)
	}
}

func TestBFSPath_SrcEqDst(t *testing.T) {
	v := bfsView()
	a := v.NodesByID["A"]
	hops := BFSPath(v, []Node{a}, map[string]bool{"A": true}, 5)
	if len(hops) != 1 {
		t.Fatalf("src==dst should return 1-hop path, got %d", len(hops))
	}
}

func TestBFSPath_DepthCapPreventsPath(t *testing.T) {
	v := bfsView()
	a := v.NodesByID["A"]
	// A→B→C is 2 hops; depth cap of 1 should not reach C.
	hops := BFSPath(v, []Node{a}, map[string]bool{"C": true}, 1)
	if hops != nil {
		t.Errorf("expected nil when depth cap prevents reaching dst, got %v", hops)
	}
}

func TestBFSPath_EmptyView(t *testing.T) {
	v := &View{
		NodesByID:        map[string]Node{},
		NodesByName:      map[string][]Node{},
		NodesByQualified: map[string][]Node{},
		NodesByPackage:   map[string][]Node{},
		NodesByPath:      map[string][]Node{},
		EdgesBySrc:       map[string][]Edge{},
		EdgesByDst:       map[string][]Edge{},
		EdgesByKind:      map[graph.EdgeKind][]Edge{},
	}
	hops := BFSPath(v, nil, map[string]bool{"X": true}, 5)
	if hops != nil {
		t.Errorf("expected nil for empty view, got %v", hops)
	}
}
