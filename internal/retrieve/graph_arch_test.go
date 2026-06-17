package retrieve

import (
	"testing"

	"github.com/alehatsman/dex/internal/graph"
	"github.com/alehatsman/dex/internal/graphquery"
)

// TestEnrichGraphArchitectureRollup is the regression for #537: the
// architecture lane used to let one package monopolize the node budget,
// rank members alphabetically, and emit zero edges — making the `avoid`
// hint ("these nodes ARE the structural overview") a lie. The rollup must
// now be a balanced, PageRank-ranked cross-section carrying real
// inter-package import edges.
func TestEnrichGraphArchitectureRollup(t *testing.T) {
	nodesByID := map[string]graphquery.Node{}
	add := func(n graphquery.Node) { nodesByID[n.ID] = n }

	aPkg := graphquery.Node{ID: "a_pkg", Kind: graph.NodePackage, Name: "a", QualifiedName: "internal/a", PackagePath: "internal/a"}
	bPkg := graphquery.Node{ID: "b_pkg", Kind: graph.NodePackage, Name: "b", QualifiedName: "internal/b", PackagePath: "internal/b"}
	add(aPkg)
	add(bPkg)

	// Package a is deliberately huge — more members than the per-package cap
	// — so the old code's first-package monopoly would starve package b.
	aMembers := []graphquery.Node{aPkg}
	const aCount = 40
	for i := 0; i < aCount; i++ {
		// Descending PageRank: f00 is the most central, f39 the least.
		n := graphquery.Node{
			ID:            "a_f" + itoa(i),
			Kind:          graph.NodeFunction,
			QualifiedName: "a.F" + itoa(i),
			PackagePath:   "internal/a",
			PageRank:      float64(aCount - i),
		}
		add(n)
		aMembers = append(aMembers, n)
	}

	bFn := graphquery.Node{ID: "b_f0", Kind: graph.NodeFunction, QualifiedName: "b.Thing", PackagePath: "internal/b", PageRank: 5}
	add(bFn)

	// Imports: a→b is internal (kept as an edge); a→fmt is external (dropped).
	impB := graphquery.Node{ID: "imp_b", Kind: graph.NodeImport, QualifiedName: "internal/b", PackagePath: "internal/a"}
	impFmt := graphquery.Node{ID: "imp_fmt", Kind: graph.NodeImport, QualifiedName: "fmt", PackagePath: "internal/a"}
	add(impB)
	add(impFmt)

	view := &graphquery.View{
		NodesByID: nodesByID,
		NodesByPackage: map[string][]graphquery.Node{
			"internal/a": append(aMembers, impB, impFmt),
			"internal/b": {bPkg, bFn},
		},
		EdgesByKind: map[graph.EdgeKind][]graphquery.Edge{
			graph.EdgeImports: {
				{Kind: graph.EdgeImports, SrcID: "a_pkg", DstID: "imp_b"},
				{Kind: graph.EdgeImports, SrcID: "a_pkg", DstID: "imp_fmt"},
			},
		},
	}

	gr, ok := EnrichGraph(IntentArchitecture, view, nil, nil)
	if !ok || gr == nil {
		t.Fatal("EnrichGraph returned no result for architecture")
	}

	present := map[string]bool{}
	for _, n := range gr.Nodes {
		present[n.ID] = true
	}

	// #537 monopoly: package b must still be anchored despite package a's flood.
	if !present[CompactID(bPkg)] {
		t.Error("package b node starved out of the rollup — budget monopoly regressed")
	}
	if !present[CompactID(aPkg)] {
		t.Error("package a node missing from the rollup")
	}

	// #537 ranking: the most central member wins the per-package cap; the
	// least central is dropped.
	top := graphquery.Node{ID: "a_f0", Kind: graph.NodeFunction, QualifiedName: "a.F" + itoa(0), PackagePath: "internal/a"}
	least := graphquery.Node{ID: "a_f" + itoa(aCount-1), Kind: graph.NodeFunction, QualifiedName: "a.F" + itoa(aCount-1), PackagePath: "internal/a"}
	if !present[CompactID(top)] {
		t.Error("highest-PageRank member dropped — ranking regressed to alphabetical")
	}
	if present[CompactID(least)] {
		t.Error("lowest-PageRank member kept past the per-package cap")
	}

	// #537 edges: the internal a→b import edge must be emitted; the external
	// fmt import must not.
	if len(gr.Edges) == 0 {
		t.Fatal("architecture rollup emitted zero edges — still a flat node list")
	}
	if present[CompactID(impFmt)] {
		t.Error("external import 'fmt' leaked into the project topology")
	}
	foundInternal := false
	for _, e := range gr.Edges {
		if e.Kind == string(graph.EdgeImports) && e.From == CompactID(aPkg) && e.To == CompactID(impB) {
			foundInternal = true
		}
	}
	if !foundInternal {
		t.Error("internal a→b import edge missing from the rollup")
	}

	if len(gr.Nodes) > MaxGraphNodes {
		t.Errorf("node budget exceeded: %d > %d", len(gr.Nodes), MaxGraphNodes)
	}
}

// TestEnrichGraphArchitecturePrefersExportedReps is the regression for #570:
// the architecture rollup ranked representatives by raw PageRank, so unexported
// implementation helpers (writeJSON, err) and trivial names (do) floated up as
// a package's "shape" instead of its exported API. Exported symbols must win
// the slots; trivial noise must be dropped; non-noise helpers only backfill.
func TestEnrichGraphArchitecturePrefersExportedReps(t *testing.T) {
	nodesByID := map[string]graphquery.Node{}
	add := func(n graphquery.Node) { nodesByID[n.ID] = n }

	cPkg := graphquery.Node{ID: "c_pkg", Kind: graph.NodePackage, Name: "c", QualifiedName: "internal/c", PackagePath: "internal/c"}
	server := graphquery.Node{ID: "c_server", Kind: graph.NodeType, QualifiedName: "c.Server", PackagePath: "internal/c", PageRank: 1}
	writeJSON := graphquery.Node{ID: "c_wj", Kind: graph.NodeFunction, QualifiedName: "c.writeJSON", PackagePath: "internal/c", PageRank: 50}
	errFn := graphquery.Node{ID: "c_err", Kind: graph.NodeFunction, QualifiedName: "c.err", PackagePath: "internal/c", PageRank: 99}
	doFn := graphquery.Node{ID: "c_do", Kind: graph.NodeFunction, QualifiedName: "(*c).do", PackagePath: "internal/c", PageRank: 99}
	for _, n := range []graphquery.Node{cPkg, server, writeJSON, errFn, doFn} {
		add(n)
	}

	view := &graphquery.View{
		NodesByID: nodesByID,
		NodesByPackage: map[string][]graphquery.Node{
			"internal/c": {cPkg, server, writeJSON, errFn, doFn},
		},
		EdgesByKind: map[graph.EdgeKind][]graphquery.Edge{},
	}

	gr, ok := EnrichGraph(IntentArchitecture, view, nil, nil)
	if !ok || gr == nil {
		t.Fatal("EnrichGraph returned no result for architecture")
	}
	present := map[string]bool{}
	for _, n := range gr.Nodes {
		present[n.ID] = true
	}

	if !present[CompactID(server)] {
		t.Error("exported Server must be kept as a representative even at low PageRank")
	}
	if present[CompactID(errFn)] {
		t.Error("noise 'err' must be dropped from architecture representatives")
	}
	if present[CompactID(doFn)] {
		t.Error("<=2-char 'do' must be dropped from architecture representatives")
	}
	if !present[CompactID(writeJSON)] {
		t.Error("non-noise unexported helper should backfill remaining slots")
	}
}

// itoa is a tiny zero-padded int formatter so member IDs sort lexically the
// same as numerically (f00..f39), keeping the fixture readable.
func itoa(i int) string {
	const digits = "0123456789"
	return string([]byte{digits[i/10], digits[i%10]})
}
