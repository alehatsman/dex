package mcp

import (
	"testing"

	"github.com/alehatsman/dex/internal/graph"
	"github.com/alehatsman/dex/internal/graphquery"
)

// callChainView builds a tiny graph: C ──calls──▶ A ──calls──▶ B.
// A lives in a.go:10-20; B in b.go:1-5; C in c.go:1-5.
func callChainView() *graphquery.View {
	v := &graphquery.View{
		NodesByID:        map[string]graphquery.Node{},
		NodesByName:      map[string][]graphquery.Node{},
		NodesByQualified: map[string][]graphquery.Node{},
		NodesByPath:      map[string][]graphquery.Node{},
		EdgesBySrc:       map[string][]graphquery.Edge{},
		EdgesByDst:       map[string][]graphquery.Edge{},
		EdgesByKind:      map[graph.EdgeKind][]graphquery.Edge{},
	}
	for _, n := range []graphquery.Node{
		{ID: "A", Kind: graph.NodeFunction, Name: "A", QualifiedName: "A", FilePath: "a.go", StartLine: 10, EndLine: 20},
		{ID: "B", Kind: graph.NodeFunction, Name: "B", QualifiedName: "B", FilePath: "b.go", StartLine: 1, EndLine: 5},
		{ID: "C", Kind: graph.NodeFunction, Name: "C", QualifiedName: "C", FilePath: "c.go", StartLine: 1, EndLine: 5},
	} {
		v.NodesByID[n.ID] = n
		v.NodesByPath[n.FilePath] = append(v.NodesByPath[n.FilePath], n)
	}
	addEdge := func(src, dst string) {
		e := graphquery.Edge{Kind: graph.EdgeCalls, SrcID: src, DstID: dst}
		v.EdgesBySrc[src] = append(v.EdgesBySrc[src], e)
		v.EdgesByDst[dst] = append(v.EdgesByDst[dst], e)
		v.EdgesByKind[graph.EdgeCalls] = append(v.EdgesByKind[graph.EdgeCalls], e)
	}
	addEdge("A", "B") // A calls B
	addEdge("C", "A") // C calls A
	return v
}

func paths(syms []SymbolHit) []string {
	out := make([]string, len(syms))
	for i, s := range syms {
		out[i] = s.Path
	}
	return out
}

// A symbol seed at a.go:12 (inside A's 10-20 range) must pull in both its
// callee (B) and its caller (C), with the seed leading the result.
func TestExpandAssemblePoolSymbolSeed(t *testing.T) {
	v := callChainView()
	seed := []SymbolHit{{QualifiedName: "A", Path: "a.go", StartLine: 12, EndLine: 12}}
	got := expandAssemblePool(v, seed, nil, 16)
	if len(got) == 0 || got[0].Path != "a.go" {
		t.Fatalf("seed must lead result, got %v", paths(got))
	}
	for _, want := range []string{"b.go", "c.go"} {
		if !contains(paths(got), want) {
			t.Errorf("neighbor %q not added; got %v", want, paths(got))
		}
	}
}

// A semantic-hit seed (no symbol-lane hits at all — the pure-prose case)
// must also expand along the call graph.
func TestExpandAssemblePoolSemanticSeed(t *testing.T) {
	v := callChainView()
	sem := []SemHit{{Path: "a.go", StartLine: 15, EndLine: 15}}
	got := expandAssemblePool(v, nil, sem, 16)
	if !contains(paths(got), "b.go") || !contains(paths(got), "c.go") {
		t.Errorf("semantic seed did not pull call-neighbors; got %v", paths(got))
	}
}

func TestExpandAssemblePoolDedup(t *testing.T) {
	v := callChainView()
	// B is already in the seed set; it must not be added a second time.
	seed := []SymbolHit{
		{QualifiedName: "A", Path: "a.go", StartLine: 12, EndLine: 12},
		{QualifiedName: "B", Path: "b.go", StartLine: 1, EndLine: 5},
	}
	got := expandAssemblePool(v, seed, nil, 16)
	n := 0
	for _, p := range paths(got) {
		if p == "b.go" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("b.go appears %d times, want 1: %v", n, paths(got))
	}
}

func TestExpandAssemblePoolCap(t *testing.T) {
	v := callChainView()
	seed := []SymbolHit{{QualifiedName: "A", Path: "a.go", StartLine: 12, EndLine: 12}}
	got := expandAssemblePool(v, seed, nil, 1)
	if len(got) != 2 { // seed + exactly one neighbor
		t.Errorf("maxAdd=1 should yield 1 seed + 1 neighbor, got %d: %v", len(got), paths(got))
	}
}

func TestExpandAssemblePoolNilView(t *testing.T) {
	seed := []SymbolHit{{QualifiedName: "A", Path: "a.go", StartLine: 12}}
	if got := expandAssemblePool(nil, seed, nil, 16); len(got) != 1 {
		t.Errorf("nil view must be a no-op, got %v", paths(got))
	}
}

func TestCoveringNode(t *testing.T) {
	v := callChainView()
	if n, ok := coveringNode(v, "a.go", 15); !ok || n.ID != "A" {
		t.Errorf("a.go:15 should resolve to A, got %v ok=%v", n.ID, ok)
	}
	if _, ok := coveringNode(v, "a.go", 99); ok {
		t.Error("a.go:99 covers no node, want ok=false")
	}
	if _, ok := coveringNode(v, "missing.go", 1); ok {
		t.Error("unknown path covers no node, want ok=false")
	}
}
