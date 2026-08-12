package retrieve

import (
	"strings"
	"testing"

	"github.com/alehatsman/dex/internal/graph"
	"github.com/alehatsman/dex/internal/graphquery"
)

// jsWorkspaceView builds a per-module TS import view: two @bright/ui modules
// importing two @bright/common modules (dedups to one project edge), plus an
// intra-project import that must roll away. Mirrors graphquery/project_test.go
// but exercises the retrieve-layer projectTopology lane end to end.
func jsWorkspaceView() *graphquery.View {
	tsPkg := func(id, path string) graphquery.Node {
		return graphquery.Node{ID: id, Kind: graph.NodePackage, PackagePath: path,
			MetadataJSON: []byte(`{"language":"typescript"}`)}
	}
	tsImp := func(id, importer, specifier, target string) graphquery.Node {
		md := `{"language":"typescript","target":"` + target + `"}`
		return graphquery.Node{ID: id, Kind: graph.NodeImport, PackagePath: importer,
			QualifiedName: specifier, MetadataJSON: []byte(md)}
	}
	edge := func(src, imp string) graphquery.Edge {
		return graphquery.Edge{Kind: graph.EdgeImports, SrcID: src, DstID: imp}
	}
	return &graphquery.View{
		NodesByID: map[string]graphquery.Node{
			"m-button": tsPkg("m-button", "packages/bright-ui/src/Button"),
			"m-theme":  tsPkg("m-theme", "packages/bright-ui/src/Theme"),
			"m-card":   tsPkg("m-card", "packages/bright-ui/src/Card"),
			"m-string": tsPkg("m-string", "packages/bright-common/src/String"),
			"m-b64":    tsPkg("m-b64", "packages/bright-common/src/Base64"),
			"i1":       tsImp("i1", "packages/bright-ui/src/Button", "@bright/common/String", "packages/bright-common/src/String"),
			"i2":       tsImp("i2", "packages/bright-ui/src/Theme", "@bright/common/Base64", "packages/bright-common/src/Base64"),
			"i3":       tsImp("i3", "packages/bright-ui/src/Card", "@bright/ui/Button", "packages/bright-ui/src/Button"), // intra-project
		},
		EdgesByKind: map[graph.EdgeKind][]graphquery.Edge{
			graph.EdgeImports: {edge("m-button", "i1"), edge("m-theme", "i2"), edge("m-card", "i3")},
		},
	}
}

func mapProjectOf(p string) string {
	switch {
	case strings.HasPrefix(p, "packages/bright-ui/"):
		return "@bright/ui"
	case strings.HasPrefix(p, "packages/bright-common/"):
		return "@bright/common"
	default:
		return ""
	}
}

// TestEnrichGraphProjectTopology: package_topology on a JS/TS workspace surfaces
// the workspace-project DAG (the #151 fix) — one node per project, cross-project
// import edges deduped, intra-project edges dropped. The semantic hits are docs
// that map to no package node (the real-world failure mode where the module lane
// returned nothing) — the project rollup must not depend on them.
func TestEnrichGraphProjectTopology(t *testing.T) {
	docHits := []SemHit{{Path: "rush.json"}, {Path: "apps/api-docs/packaging/recipe.rb"}}

	gr, ok := EnrichGraph(IntentPackageTopology, jsWorkspaceView(), docHits, nil, mapProjectOf)
	if !ok || gr == nil {
		t.Fatalf("EnrichGraph ok=%v gr=%v, want a project-rollup graph", ok, gr)
	}

	gotNodes := map[string]string{}
	for _, n := range gr.Nodes {
		gotNodes[n.ID] = n.Kind
	}
	for _, proj := range []string{"@bright/ui", "@bright/common"} {
		if gotNodes[proj] != string(graph.NodePackage) {
			t.Errorf("node %q kind = %q, want %q", proj, gotNodes[proj], graph.NodePackage)
		}
	}
	if len(gr.Nodes) != 2 {
		t.Errorf("nodes = %d, want 2 (%+v)", len(gr.Nodes), gr.Nodes)
	}

	if len(gr.Edges) != 1 {
		t.Fatalf("edges = %d, want 1 deduped cross-project edge (%+v)", len(gr.Edges), gr.Edges)
	}
	e := gr.Edges[0]
	if e.From != "@bright/ui" || e.To != "@bright/common" || e.Kind != string(graph.EdgeImports) {
		t.Errorf("edge = %+v, want @bright/ui -imports-> @bright/common", e)
	}
}

// TestEnrichGraphProjectTopologyNilMapperFallsBack: with no projectOf (Go repo,
// or no root), the project rollup is disabled and package_topology falls back to
// the module lane — it must never emit project-name nodes. The doc-only semantic
// hits map to no package node, so the module lane produces nothing here; the
// point is only that the rollup did not fire.
func TestEnrichGraphProjectTopologyNilMapperFallsBack(t *testing.T) {
	docHits := []SemHit{{Path: "rush.json"}}

	gr, _ := EnrichGraph(IntentPackageTopology, jsWorkspaceView(), docHits, nil, nil)
	if gr != nil {
		for _, n := range gr.Nodes {
			if strings.HasPrefix(n.ID, "@bright/") {
				t.Errorf("nil projectOf still emitted project node %q — rollup must be gated on the mapper", n.ID)
			}
		}
	}
}
