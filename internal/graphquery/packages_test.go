package graphquery

import (
	"testing"

	"github.com/alehatsman/dex/internal/graph"
)

// TestBuildPackageGraph drives BuildPackageGraph off a hand-built view mixing
// Go and tree-sitter (JS/TS) packages. The Go side chains a → b → c with an
// external "fmt" import that must be dropped. The tree-sitter side is a
// workspace-resolved pair web/x → web/y (dst import carries Metadata["target"]),
// which — since #127 — now appears as a real cross-module edge; a bare "react"
// import (external, no target) is dropped, and an isolated tree-sitter module
// (web/z) is not emitted. Degrees, is_main (Go-only), and PageRank ordering hold.
func TestBuildPackageGraph(t *testing.T) {
	pkg := func(id, path, name string) Node {
		return Node{ID: id, Kind: graph.NodePackage, Name: name, PackagePath: path}
	}
	imp := func(id, importer, imported string) Node {
		return Node{ID: id, Kind: graph.NodeImport, PackagePath: importer, QualifiedName: imported}
	}
	// tsPkg is a tree-sitter package node (per-file module), language-stamped.
	tsPkg := func(id, path string) Node {
		return Node{
			ID: id, Kind: graph.NodePackage, PackagePath: path,
			MetadataJSON: []byte(`{"language":"typescript"}`),
		}
	}
	// tsImp is a tree-sitter import node: the raw specifier lives in
	// QualifiedName, the resolved-internal target (if any) in Metadata["target"].
	tsImp := func(id, importer, specifier, target string) Node {
		md := `{"language":"typescript"}`
		if target != "" {
			md = `{"language":"typescript","target":"` + target + `"}`
		}
		return Node{
			ID: id, Kind: graph.NodeImport, PackagePath: importer,
			QualifiedName: specifier, MetadataJSON: []byte(md),
		}
	}
	edge := func(srcPkgID, impID string) Edge {
		return Edge{Kind: graph.EdgeImports, SrcID: srcPkgID, DstID: impID}
	}
	view := &View{
		NodesByID: map[string]Node{
			"pa":     pkg("pa", "mod/a", "main"), // executable entry point
			"pb":     pkg("pb", "mod/b", "b"),
			"pc":     pkg("pc", "mod/c", "c"),
			"ia-b":   imp("ia-b", "mod/a", "mod/b"),
			"ia-fmt": imp("ia-fmt", "mod/a", "fmt"), // external — dropped
			"ib-c":   imp("ib-c", "mod/b", "mod/c"),
			// tree-sitter workspace pair: web/x → web/y is resolved (has target).
			"tx":    tsPkg("tx", "web/x"),
			"ty":    tsPkg("ty", "web/y"),
			"tz":    tsPkg("tz", "web/z"), // isolated module — not emitted
			"tx-y":  tsImp("tx-y", "web/x", "@acme/y", "web/y"),
			"tx-rc": tsImp("tx-rc", "web/x", "react", ""), // external — dropped
		},
		EdgesByKind: map[graph.EdgeKind][]Edge{
			graph.EdgeImports: {
				edge("pa", "ia-b"),
				edge("pa", "ia-fmt"), // external — dropped
				edge("pb", "ib-c"),
				edge("tx", "tx-y"),  // resolved cross-module edge — kept
				edge("tx", "tx-rc"), // external — dropped
			},
		},
	}

	out := BuildPackageGraph(view)
	if len(out.Nodes) == 0 {
		t.Fatalf("got empty package graph, want populated")
	}

	// Edges: internal-only, sorted by (from, to). The tree-sitter resolved edge
	// now appears alongside the Go edges.
	wantEdges := []PackageImport{
		{FromPackage: "mod/a", ToPackage: "mod/b"},
		{FromPackage: "mod/b", ToPackage: "mod/c"},
		{FromPackage: "web/x", ToPackage: "web/y"},
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
	// Emitted: 3 Go packages (always) + the 2 participating tree-sitter modules.
	// The isolated web/z is NOT emitted; external "react"/"fmt" never appear.
	if len(out.Nodes) != 5 {
		t.Fatalf("nodes = %d, want 5 (%+v)", len(out.Nodes), out.Nodes)
	}
	if _, leaked := byPkg["web/z"]; leaked {
		t.Errorf("isolated tree-sitter module web/z leaked into the DAG")
	}
	for pkg, want := range map[string][2]int{
		"mod/a": {0, 1}, // {in, out}
		"mod/b": {1, 1},
		"mod/c": {1, 0},
		"web/x": {0, 1},
		"web/y": {1, 0},
	} {
		got := byPkg[pkg]
		if got.InDegree != want[0] || got.OutDegree != want[1] {
			t.Errorf("%s: in=%d out=%d, want in=%d out=%d", pkg, got.InDegree, got.OutDegree, want[0], want[1])
		}
	}

	// is_main marks the `package main` executable (mod/a) and only it — never a
	// tree-sitter module, even one named "main".
	for pkg, wantMain := range map[string]bool{
		"mod/a": true, "mod/b": false, "mod/c": false, "web/x": false, "web/y": false,
	} {
		if got := byPkg[pkg].IsMain; got != wantMain {
			t.Errorf("%s: IsMain=%v, want %v", pkg, got, wantMain)
		}
	}

	// Go PageRank flows importer → imported: foundation (c) > middle (b) > entry
	// (a). The disconnected tree-sitter component doesn't change that ordering.
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

// Node.Language follows the extractor convention: tree-sitter stamps
// Metadata["language"]; the Go extractor leaves it absent. So missing,
// unparseable, or language-less metadata reads as Go (#582).
func TestNodeLanguage(t *testing.T) {
	cases := []struct {
		name string
		md   []byte
		want string
	}{
		{"no metadata", nil, "go"},
		{"empty metadata", []byte{}, "go"},
		{"typescript", []byte(`{"language":"typescript"}`), "typescript"},
		{"python", []byte(`{"language":"python","receiver":"C"}`), "python"},
		{"unparseable", []byte(`{not json`), "go"},
		{"no language key", []byte(`{"receiver":"C"}`), "go"},
		{"empty language value", []byte(`{"language":""}`), "go"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := (Node{MetadataJSON: c.md}).Language(); got != c.want {
				t.Errorf("Language() = %q, want %q", got, c.want)
			}
		})
	}
}
