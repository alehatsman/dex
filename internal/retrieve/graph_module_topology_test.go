package retrieve

import (
	"strings"
	"testing"

	"github.com/alehatsman/dex/internal/graph"
	"github.com/alehatsman/dex/internal/graphquery"
)

// goModuleView builds a small Go import DAG: a imports b and c; b imports c.
// So fan-in ranks c (in=2) over b (in=1) over a (in=0). A JS/TS test fixture
// package under testdata/ is wired with a resolved intra-fixture edge to prove
// BuildPackageGraph's #181 exclusion carries through the topology lane.
func goModuleView() *graphquery.View {
	goPkg := func(id, path string) graphquery.Node {
		return graphquery.Node{ID: id, Kind: graph.NodePackage, Name: id, PackagePath: path}
	}
	goImp := func(id, importer, target string) graphquery.Node {
		return graphquery.Node{ID: id, Kind: graph.NodeImport, PackagePath: importer, QualifiedName: target}
	}
	edge := func(src, imp string) graphquery.Edge {
		return graphquery.Edge{Kind: graph.EdgeImports, SrcID: src, DstID: imp}
	}
	nodes := map[string]graphquery.Node{
		"a": goPkg("a", "mod/a"),
		"b": goPkg("b", "mod/b"),
		"c": goPkg("c", "mod/c"),
		// A buried JS/TS fixture package + a resolved intra-fixture edge — must
		// never surface in the module DAG (#181).
		"fx-a": {ID: "fx-a", Kind: graph.NodePackage, PackagePath: "internal/graph/testdata/ts/src/a", MetadataJSON: []byte(`{"language":"typescript"}`)},
		"fx-b": {ID: "fx-b", Kind: graph.NodePackage, PackagePath: "internal/graph/testdata/ts/src/b", MetadataJSON: []byte(`{"language":"typescript"}`)},
		"fx-i": {ID: "fx-i", Kind: graph.NodeImport, PackagePath: "internal/graph/testdata/ts/src/a", QualifiedName: "./b", MetadataJSON: []byte(`{"language":"typescript","target":"internal/graph/testdata/ts/src/b"}`)},
		"ia-b": goImp("ia-b", "mod/a", "mod/b"),
		"ia-c": goImp("ia-c", "mod/a", "mod/c"),
		"ib-c": goImp("ib-c", "mod/b", "mod/c"),
	}
	byPkg := map[string][]graphquery.Node{}
	for _, n := range nodes {
		if n.Kind == graph.NodePackage {
			byPkg[n.PackagePath] = append(byPkg[n.PackagePath], n)
		}
	}
	return &graphquery.View{
		NodesByID:      nodes,
		NodesByPackage: byPkg,
		NodesByPath:    map[string][]graphquery.Node{},
		EdgesByKind: map[graph.EdgeKind][]graphquery.Edge{
			graph.EdgeImports: {edge("a", "ia-b"), edge("a", "ia-c"), edge("b", "ib-c"), edge("fx-a", "fx-i")},
		},
	}
}

// TestEnrichGraphModuleTopology: package_topology on a Go repo (nil projectOf)
// surfaces the WHOLE module DAG ranked by import fan-in — not the semantic
// neighborhood — with in/out-degree + PageRank on each node (#190). Passing no
// semHits proves the answer is independent of what the semantic lane surfaced,
// which was the old lane's defect.
func TestEnrichGraphModuleTopology(t *testing.T) {
	gr, ok := EnrichGraph(IntentPackageTopology, goModuleView(), nil, nil, nil)
	if !ok || gr == nil {
		t.Fatalf("EnrichGraph ok=%v gr=%v, want the module DAG", ok, gr)
	}

	byID := map[string]GraphNode{}
	for _, n := range gr.Nodes {
		byID[n.ID] = n
		if n.PageRank <= 0 {
			t.Errorf("node %q has non-positive PageRank %v — topology nodes must carry centrality", n.ID, n.PageRank)
		}
	}

	// All three real packages present despite zero semHits.
	for _, id := range []string{"a", "b", "c"} {
		if _, ok := byID[id]; !ok {
			t.Errorf("package %q missing from module DAG (%+v)", id, gr.Nodes)
		}
	}
	// Fan-in is carried and correct: c imported by a+b, b by a, a by nobody.
	if got := byID["c"].InDegree; got != 2 {
		t.Errorf("c in-degree = %d, want 2", got)
	}
	if got := byID["b"].InDegree; got != 1 {
		t.Errorf("b in-degree = %d, want 1", got)
	}
	if got := byID["a"].OutDegree; got != 2 {
		t.Errorf("a out-degree = %d, want 2", got)
	}
	// Ranked in-degree descending: the load-bearing package leads.
	if gr.Nodes[0].ID != "c" {
		t.Errorf("first node = %q, want c (highest fan-in) — order is the ranking (%+v)", gr.Nodes[0].ID, gr.Nodes)
	}
	// The full path rides along as QualifiedName since the compact ID is the tail.
	if byID["c"].QualifiedName != "mod/c" {
		t.Errorf("c qualified_name = %q, want mod/c", byID["c"].QualifiedName)
	}

	// #181 exclusion holds through the topology lane: exactly the 3 real
	// packages, no testdata fixture leaks.
	if len(gr.Nodes) != 3 {
		t.Errorf("node count = %d, want exactly 3 real packages (%+v)", len(gr.Nodes), gr.Nodes)
	}
	for _, n := range gr.Nodes {
		if strings.Contains(n.QualifiedName, "testdata") {
			t.Errorf("fixture package leaked into module DAG: %+v", n)
		}
	}
}
