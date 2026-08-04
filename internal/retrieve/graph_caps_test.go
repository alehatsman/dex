package retrieve

import (
	"fmt"
	"testing"

	"github.com/alehatsman/dex/internal/graph"
	"github.com/alehatsman/dex/internal/graphquery"
)

// The cases below moved down from internal/mcp when #114 deleted the
// enrichGraph transport wrapper: the neighborhood expansion and its caps live
// here, so the tests call EnrichGraph directly and assert on the returned
// GraphResult rather than the wire ContextOutput the wrapper populated.

// TestEnrichGraphCaps guards against unbounded graph payloads — the
// regression that motivated MaxGraphNodes/MaxGraphEdges. A big
// package's rollup or a god-struct's sibling fan-out should not blow
// the response budget.
func TestEnrichGraphCaps(t *testing.T) {
	t.Run("node cap via package rollup", func(t *testing.T) {
		view := &graphquery.View{
			NodesByID:        map[string]graphquery.Node{},
			NodesByName:      map[string][]graphquery.Node{},
			NodesByQualified: map[string][]graphquery.Node{},
			NodesByPackage:   map[string][]graphquery.Node{},
			NodesByPath:      map[string][]graphquery.Node{},
			EdgesBySrc:       map[string][]graphquery.Edge{},
			EdgesByDst:       map[string][]graphquery.Edge{},
			EdgesByKind:      map[graph.EdgeKind][]graphquery.Edge{},
		}
		const pkg = "example.com/bigpkg"
		for i := range 100 {
			n := graphquery.Node{
				ID:            fmt.Sprintf("n%d", i),
				Kind:          graph.NodeFunction,
				Name:          fmt.Sprintf("Fn%d", i),
				QualifiedName: fmt.Sprintf("Fn%d", i),
				PackagePath:   pkg,
				FilePath:      "bigpkg/bigpkg.go",
			}
			view.NodesByID[n.ID] = n
			view.NodesByPackage[pkg] = append(view.NodesByPackage[pkg], n)
			view.NodesByPath[n.FilePath] = append(view.NodesByPath[n.FilePath], n)
		}
		gr, ok := EnrichGraph(IntentArchitecture, view, []SemHit{{Path: "bigpkg/bigpkg.go"}}, nil)
		if !ok || gr == nil {
			t.Fatal("expected a rollup from the package flood")
		}
		if got := len(gr.Nodes); got > MaxGraphNodes {
			t.Errorf("got %d nodes, want ≤ %d", got, MaxGraphNodes)
		}
		if len(gr.Nodes) == 0 {
			t.Error("expected some nodes from package rollup")
		}
	})

	t.Run("edge cap via package_topology imports", func(t *testing.T) {
		view := &graphquery.View{
			NodesByID:        map[string]graphquery.Node{},
			NodesByName:      map[string][]graphquery.Node{},
			NodesByQualified: map[string][]graphquery.Node{},
			NodesByPackage:   map[string][]graphquery.Node{},
			NodesByPath:      map[string][]graphquery.Node{},
			EdgesBySrc:       map[string][]graphquery.Edge{},
			EdgesByDst:       map[string][]graphquery.Edge{},
			EdgesByKind:      map[graph.EdgeKind][]graphquery.Edge{},
		}
		const src = "example.com/src"
		srcPkg := graphquery.Node{ID: "src", Kind: graph.NodePackage, Name: "src", PackagePath: src, FilePath: "src/src.go"}
		view.NodesByID[srcPkg.ID] = srcPkg
		view.NodesByPackage[src] = append(view.NodesByPackage[src], srcPkg)
		view.NodesByPath[srcPkg.FilePath] = append(view.NodesByPath[srcPkg.FilePath], srcPkg)
		for i := range 100 {
			dstID := fmt.Sprintf("dst%d", i)
			dst := graphquery.Node{ID: dstID, Kind: graph.NodePackage, Name: dstID, PackagePath: "example.com/" + dstID}
			view.NodesByID[dstID] = dst
			e := graphquery.Edge{Kind: graph.EdgeImports, SrcID: srcPkg.ID, DstID: dstID}
			view.EdgesByKind[graph.EdgeImports] = append(view.EdgesByKind[graph.EdgeImports], e)
			view.EdgesBySrc[srcPkg.ID] = append(view.EdgesBySrc[srcPkg.ID], e)
			view.EdgesByDst[dstID] = append(view.EdgesByDst[dstID], e)
		}
		gr, ok := EnrichGraph(IntentPackageTopology, view, []SemHit{{Path: "src/src.go"}}, nil)
		if !ok || gr == nil {
			t.Fatal("expected a rollup from the imports flood")
		}
		if got := len(gr.Edges); got > MaxGraphEdges {
			t.Errorf("got %d edges, want ≤ %d", got, MaxGraphEdges)
		}
		if len(gr.Edges) == 0 {
			t.Error("expected some edges from imports rollup")
		}
	})
}

// TestArchitectureAnchorsOnPageRank guards the fix for the degenerate
// case where a docs-dominated semantic lane collapses the architecture
// rollup to whichever single Go file leaked through. With PageRank
// anchoring the rollup must surface the project's central packages
// even when semHits point only at non-Go paths.
func TestArchitectureAnchorsOnPageRank(t *testing.T) {
	view := &graphquery.View{
		NodesByID:        map[string]graphquery.Node{},
		NodesByName:      map[string][]graphquery.Node{},
		NodesByQualified: map[string][]graphquery.Node{},
		NodesByPackage:   map[string][]graphquery.Node{},
		NodesByPath:      map[string][]graphquery.Node{},
		EdgesBySrc:       map[string][]graphquery.Edge{},
		EdgesByDst:       map[string][]graphquery.Edge{},
		EdgesByKind:      map[graph.EdgeKind][]graphquery.Edge{},
	}
	// Three packages, descending centrality: core (hub), mid, tangent.
	type pkgSpec struct {
		path string
		file string
		pr   float64
	}
	specs := []pkgSpec{
		{"example.com/core", "core/core.go", 0.9},
		{"example.com/mid", "mid/mid.go", 0.4},
		{"example.com/tangent", "tangent/tangent.go", 0.05},
	}
	for _, s := range specs {
		pkgNode := graphquery.Node{
			ID: s.path + "::pkg", Kind: graph.NodePackage,
			Name: PkgTail(s.path), PackagePath: s.path,
			FilePath: s.file, PageRank: s.pr,
		}
		view.NodesByID[pkgNode.ID] = pkgNode
		view.NodesByPackage[s.path] = append(view.NodesByPackage[s.path], pkgNode)
		view.NodesByPath[s.file] = append(view.NodesByPath[s.file], pkgNode)
	}
	// semHits point only at a doc file — no graph nodes there.
	gr, ok := EnrichGraph(IntentArchitecture, view, []SemHit{{Path: "README.md"}}, nil)
	if !ok || gr == nil {
		t.Fatal("expected graph rollup anchored on PageRank when semHits are docs")
	}
	got := map[string]bool{}
	for _, n := range gr.Nodes {
		got[n.ID] = true
	}
	for _, want := range []string{"core", "mid", "tangent"} {
		if !got[want] {
			t.Errorf("expected pkg %q in rollup; got nodes %v", want, gr.Nodes)
		}
	}
}

// TestArchitectureAnchorAugmentedBySemHits verifies that the PageRank
// anchor union still pulls in subsystem-specific packages when the user
// names one. The architecture rollup should be hub ∪ requested.
func TestArchitectureAnchorAugmentedBySemHits(t *testing.T) {
	view := &graphquery.View{
		NodesByID:        map[string]graphquery.Node{},
		NodesByName:      map[string][]graphquery.Node{},
		NodesByQualified: map[string][]graphquery.Node{},
		NodesByPackage:   map[string][]graphquery.Node{},
		NodesByPath:      map[string][]graphquery.Node{},
		EdgesBySrc:       map[string][]graphquery.Edge{},
		EdgesByDst:       map[string][]graphquery.Edge{},
		EdgesByKind:      map[graph.EdgeKind][]graphquery.Edge{},
	}
	hub := graphquery.Node{
		ID: "hub::pkg", Kind: graph.NodePackage,
		Name: "hub", PackagePath: "example.com/hub",
		FilePath: "hub/hub.go", PageRank: 0.9,
	}
	leaf := graphquery.Node{
		ID: "leaf::pkg", Kind: graph.NodePackage,
		Name: "leaf", PackagePath: "example.com/leaf",
		FilePath: "leaf/leaf.go", PageRank: 0, // no centrality
	}
	for _, n := range []graphquery.Node{hub, leaf} {
		view.NodesByID[n.ID] = n
		view.NodesByPackage[n.PackagePath] = append(view.NodesByPackage[n.PackagePath], n)
		view.NodesByPath[n.FilePath] = append(view.NodesByPath[n.FilePath], n)
	}
	gr, ok := EnrichGraph(IntentArchitecture, view, []SemHit{{Path: "leaf/leaf.go"}}, nil)
	if !ok || gr == nil {
		t.Fatal("expected graph rollup")
	}
	got := map[string]bool{}
	for _, n := range gr.Nodes {
		got[n.ID] = true
	}
	if !got["hub"] {
		t.Error("expected hub package via PageRank anchor")
	}
	if !got["leaf"] {
		t.Error("expected leaf package via semHit augmentation")
	}
}
