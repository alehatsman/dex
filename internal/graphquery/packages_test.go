package graphquery

import (
	"testing"

	"github.com/alehatsman/dex/internal/graph"
)

// TestBuildPackageGraph drives buildPackageGraph off a hand-built view:
// three internal packages chained a → b → c, plus an external import
// ("fmt") hanging off a. The external import must not become a node or
// an edge, degrees come from the internal import edges, and PageRank
// flows importer → imported so the foundation outranks the entry point.
func TestBuildPackageGraph(t *testing.T) {
	pkg := func(id, path, name string) Node {
		return Node{ID: id, Kind: graph.NodePackage, Name: name, PackagePath: path}
	}
	imp := func(id, importer, imported string) Node {
		return Node{ID: id, Kind: graph.NodeImport, PackagePath: importer, QualifiedName: imported}
	}
	edge := func(srcPkgID, impID string) Edge {
		return Edge{Kind: graph.EdgeImports, SrcID: srcPkgID, DstID: impID}
	}
	// A non-Go (tree-sitter) package node, stamped with its language — as a
	// python/js/ts testdata fixture or web/src module would be. These form a
	// real LINKED sub-graph (py.a → py.b) but must be excluded from the Go DAG.
	sitterPkg := func(id, path, lang string) Node {
		return Node{
			ID: id, Kind: graph.NodePackage, PackagePath: path,
			MetadataJSON: []byte(`{"language":"` + lang + `"}`),
		}
	}
	view := &View{
		NodesByID: map[string]Node{
			"pa":     pkg("pa", "mod/a", "main"), // executable entry point
			"pb":     pkg("pb", "mod/b", "b"),
			"pc":     pkg("pc", "mod/c", "c"),
			"ia-b":   imp("ia-b", "mod/a", "mod/b"),
			"ia-fmt": imp("ia-fmt", "mod/a", "fmt"),
			"ib-c":   imp("ib-c", "mod/b", "mod/c"),
			// non-Go fixture pair: linked to each other, must not appear.
			"py-a":  sitterPkg("py-a", "py.a", "python"),
			"py-b":  sitterPkg("py-b", "py.b", "python"),
			"ipy-b": imp("ipy-b", "py.a", "py.b"),
		},
		EdgesByKind: map[graph.EdgeKind][]Edge{
			graph.EdgeImports: {
				edge("pa", "ia-b"),
				edge("pa", "ia-fmt"), // external — dropped
				edge("pb", "ib-c"),
				edge("py-a", "ipy-b"), // non-Go — dropped
			},
		},
	}

	out := BuildPackageGraph(view)
	if len(out.Nodes) == 0 {
		t.Fatalf("got empty package graph, want populated")
	}

	// Edges: internal-only (no fmt), sorted by (from, to).
	wantEdges := []PackageImport{
		{FromPackage: "mod/a", ToPackage: "mod/b"},
		{FromPackage: "mod/b", ToPackage: "mod/c"},
	}
	if len(out.Edges) != len(wantEdges) {
		t.Fatalf("edges = %+v, want %+v", out.Edges, wantEdges)
	}
	for i, e := range out.Edges {
		if e != wantEdges[i] {
			t.Errorf("edge[%d] = %+v, want %+v", i, e, wantEdges[i])
		}
	}

	byPkg := map[string]PackageStat{}
	for _, n := range out.Nodes {
		byPkg[n.Package] = n
	}
	// The non-Go fixture packages are excluded entirely, even though they're
	// linked to each other — only the 3 Go packages remain.
	if len(out.Nodes) != 3 {
		t.Fatalf("nodes = %d, want 3 (%+v)", len(out.Nodes), out.Nodes)
	}
	if _, leaked := byPkg["py.a"]; leaked {
		t.Errorf("non-Go package py.a leaked into the Go package graph")
	}
	for pkg, want := range map[string][2]int{
		"mod/a": {0, 1}, // {in, out}
		"mod/b": {1, 1},
		"mod/c": {1, 0},
	} {
		got := byPkg[pkg]
		if got.InDegree != want[0] || got.OutDegree != want[1] {
			t.Errorf("%s: in=%d out=%d, want in=%d out=%d", pkg, got.InDegree, got.OutDegree, want[0], want[1])
		}
	}

	// is_main marks the `package main` executable (mod/a) and only it —
	// in_degree==0 alone can't tell the entry point from an orphan helper.
	for pkg, wantMain := range map[string]bool{"mod/a": true, "mod/b": false, "mod/c": false} {
		if got := byPkg[pkg].IsMain; got != wantMain {
			t.Errorf("%s: IsMain=%v, want %v", pkg, got, wantMain)
		}
	}

	// Sorted by in-degree desc, then path: b(1), c(1), a(0).
	if got := []string{out.Nodes[0].Package, out.Nodes[1].Package, out.Nodes[2].Package}; got[0] != "mod/b" || got[1] != "mod/c" || got[2] != "mod/a" {
		t.Errorf("node order = %v, want [mod/b mod/c mod/a]", got)
	}

	// PageRank flows importer → imported, so the foundation (c) outranks
	// the middle (b) which outranks the entry point (a).
	a, b, c := byPkg["mod/a"].PageRank, byPkg["mod/b"].PageRank, byPkg["mod/c"].PageRank
	if !(c > b && b > a) {
		t.Errorf("PageRank should be c > b > a; got a=%.4f b=%.4f c=%.4f", a, b, c)
	}
}

// TestBuildPackageGraphNoPackages: a view with no package nodes (e.g. a
// non-Go repo whose graph has only file/function nodes) yields an empty
// graph so the consumer can fall back to its flat listing (the transport
// maps an empty Nodes slice to "no-graph").
func TestBuildPackageGraphNoPackages(t *testing.T) {
	view := &View{
		NodesByID: map[string]Node{
			"f": {ID: "f", Kind: graph.NodeFunction, Name: "Foo"},
		},
		EdgesByKind: map[graph.EdgeKind][]Edge{},
	}
	if out := BuildPackageGraph(view); len(out.Nodes) != 0 {
		t.Errorf("nodes = %d, want 0 (no-graph)", len(out.Nodes))
	}
}
