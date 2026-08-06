package graphquery

import (
	"strings"
	"testing"

	"github.com/alehatsman/dex/internal/graph"
)

// TestBuildProjectGraph rolls a per-module view up to workspace projects: two
// bright-ui modules and one bright-common module. Cross-project imports collapse
// to a single @bright/ui → @bright/common edge (dedup across two source files);
// an intra-project import is dropped; a module owned by no project is dropped;
// an external import (no target) is dropped.
func TestBuildProjectGraph(t *testing.T) {
	tsPkg := func(id, path string) Node {
		return Node{ID: id, Kind: graph.NodePackage, PackagePath: path,
			MetadataJSON: []byte(`{"language":"typescript"}`)}
	}
	tsImp := func(id, importer, specifier, target string) Node {
		md := `{"language":"typescript"}`
		if target != "" {
			md = `{"language":"typescript","target":"` + target + `"}`
		}
		return Node{ID: id, Kind: graph.NodeImport, PackagePath: importer,
			QualifiedName: specifier, MetadataJSON: []byte(md)}
	}
	edge := func(src, imp string) Edge { return Edge{Kind: graph.EdgeImports, SrcID: src, DstID: imp} }

	view := &View{
		NodesByID: map[string]Node{
			"m-button": tsPkg("m-button", "packages/bright-ui/src/Button"),
			"m-theme":  tsPkg("m-theme", "packages/bright-ui/src/Theme"),
			"m-card":   tsPkg("m-card", "packages/bright-ui/src/Card"),
			"m-string": tsPkg("m-string", "packages/bright-common/src/String"),
			"m-b64":    tsPkg("m-b64", "packages/bright-common/src/Base64"),
			"m-app":    tsPkg("m-app", "apps/base-view/src/main"), // owned by no project
			// two ui→common imports (different files) — must dedup to one edge
			"i1": tsImp("i1", "packages/bright-ui/src/Button", "@bright/common/String", "packages/bright-common/src/String"),
			"i2": tsImp("i2", "packages/bright-ui/src/Theme", "@bright/common/Base64", "packages/bright-common/src/Base64"),
			// intra-project (ui→ui) — dropped
			"i3": tsImp("i3", "packages/bright-ui/src/Card", "@bright/ui/Button", "packages/bright-ui/src/Button"),
			// unowned importer (apps/base-view not in mapper) — dropped
			"i4": tsImp("i4", "apps/base-view/src/main", "@bright/ui/Button", "packages/bright-ui/src/Button"),
			// external, no target — dropped
			"i5": tsImp("i5", "packages/bright-ui/src/Button", "react", ""),
		},
		EdgesByKind: map[graph.EdgeKind][]Edge{
			graph.EdgeImports: {
				edge("m-button", "i1"),
				edge("m-theme", "i2"),
				edge("m-card", "i3"),
				edge("m-app", "i4"),
				edge("m-button", "i5"),
			},
		},
	}

	// Map-backed mapper stands in for resolve.Workspace.ProjectOf — keeps the
	// test pure (no disk).
	projectOf := func(p string) string {
		switch {
		case strings.HasPrefix(p, "packages/bright-ui/"):
			return "@bright/ui"
		case strings.HasPrefix(p, "packages/bright-common/"):
			return "@bright/common"
		default:
			return ""
		}
	}

	out := BuildProjectGraph(view, projectOf)

	wantEdges := []PackageImport{{FromPackage: "@bright/ui", ToPackage: "@bright/common"}}
	if len(out.Edges) != len(wantEdges) || out.Edges[0] != wantEdges[0] {
		t.Fatalf("edges = %+v, want %+v (intra-project/unowned/external/dup must drop)", out.Edges, wantEdges)
	}

	byPkg := map[string]PackageStat{}
	for _, n := range out.Nodes {
		byPkg[n.Package] = n
	}
	if len(out.Nodes) != 2 {
		t.Fatalf("nodes = %d, want 2 (%+v)", len(out.Nodes), out.Nodes)
	}
	for proj, want := range map[string][2]int{
		"@bright/ui":     {0, 1}, // {in, out}
		"@bright/common": {1, 0},
	} {
		got := byPkg[proj]
		if got.InDegree != want[0] || got.OutDegree != want[1] {
			t.Errorf("%s: in=%d out=%d, want in=%d out=%d", proj, got.InDegree, got.OutDegree, want[0], want[1])
		}
		if got.IsMain {
			t.Errorf("%s: IsMain=true, want false (projects have no main notion)", proj)
		}
	}

	// PageRank flows importer → imported: the foundation project outranks the app.
	if ui, common := byPkg["@bright/ui"].PageRank, byPkg["@bright/common"].PageRank; !(common > ui) {
		t.Errorf("PageRank should be @bright/common > @bright/ui; got ui=%.4f common=%.4f", ui, common)
	}
}

// TestBuildProjectGraphEmpty: nil view / nil mapper / no cross-project edges all
// yield an empty graph (the transport maps that to no-graph).
func TestBuildProjectGraphEmpty(t *testing.T) {
	if out := BuildProjectGraph(nil, func(string) string { return "" }); len(out.Nodes) != 0 {
		t.Errorf("nil view: nodes = %d, want 0", len(out.Nodes))
	}
	view := &View{
		NodesByID:   map[string]Node{"m": {ID: "m", Kind: graph.NodePackage, PackagePath: "packages/x/src/a"}},
		EdgesByKind: map[graph.EdgeKind][]Edge{},
	}
	if out := BuildProjectGraph(view, nil); len(out.Nodes) != 0 {
		t.Errorf("nil mapper: nodes = %d, want 0", len(out.Nodes))
	}
}
