package mcp

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
	pkg := func(id, path string) graphNode {
		return graphNode{ID: id, Kind: graph.NodePackage, PackagePath: path}
	}
	imp := func(id, importer, imported string) graphNode {
		return graphNode{ID: id, Kind: graph.NodeImport, PackagePath: importer, QualifiedName: imported}
	}
	edge := func(srcPkgID, impID string) graphEdge {
		return graphEdge{Kind: graph.EdgeImports, SrcID: srcPkgID, DstID: impID}
	}
	view := &graphView{
		nodesByID: map[string]graphNode{
			"pa":     pkg("pa", "mod/a"),
			"pb":     pkg("pb", "mod/b"),
			"pc":     pkg("pc", "mod/c"),
			"ia-b":   imp("ia-b", "mod/a", "mod/b"),
			"ia-fmt": imp("ia-fmt", "mod/a", "fmt"),
			"ib-c":   imp("ib-c", "mod/b", "mod/c"),
		},
		edgesByKind: map[graph.EdgeKind][]graphEdge{
			graph.EdgeImports: {
				edge("pa", "ia-b"),
				edge("pa", "ia-fmt"), // external — dropped
				edge("pb", "ib-c"),
			},
		},
	}

	out := buildPackageGraph(view)
	if out.Status != "ok" {
		t.Fatalf("status = %q, want ok (hint=%q)", out.Status, out.Hint)
	}

	// Edges: internal-only (no fmt), sorted by (from, to).
	wantEdges := []PackageEdge{
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

	byPkg := map[string]PackageNode{}
	for _, n := range out.Nodes {
		byPkg[n.Package] = n
	}
	if len(out.Nodes) != 3 {
		t.Fatalf("nodes = %d, want 3 (%+v)", len(out.Nodes), out.Nodes)
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
// non-Go repo whose graph has only file/function nodes) degrades to
// no-graph so the consumer can fall back to its flat listing.
func TestBuildPackageGraphNoPackages(t *testing.T) {
	view := &graphView{
		nodesByID: map[string]graphNode{
			"f": {ID: "f", Kind: graph.NodeFunction, Name: "Foo"},
		},
		edgesByKind: map[graph.EdgeKind][]graphEdge{},
	}
	if out := buildPackageGraph(view); out.Status != "no-graph" {
		t.Errorf("status = %q, want no-graph", out.Status)
	}
}
