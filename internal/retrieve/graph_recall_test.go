package retrieve

import (
	"testing"

	"github.com/alehatsman/dex/internal/graph"
	"github.com/alehatsman/dex/internal/graphquery"
)

// recallView builds C ──calls──▶ A with the name/qualified lookups populated so
// callsExpansion can seed on "A" and surface the inbound C→A call edge. langC
// stamps C's language ("" leaves metadata absent → Node.Language() == "go").
func recallView(langC string) *graphquery.View {
	v := &graphquery.View{
		NodesByID:        map[string]graphquery.Node{},
		NodesByName:      map[string][]graphquery.Node{},
		NodesByQualified: map[string][]graphquery.Node{},
		NodesByPath:      map[string][]graphquery.Node{},
		EdgesBySrc:       map[string][]graphquery.Edge{},
		EdgesByDst:       map[string][]graphquery.Edge{},
		EdgesByKind:      map[graph.EdgeKind][]graphquery.Edge{},
	}
	c := graphquery.Node{ID: "C", Kind: graph.NodeFunction, Name: "C", QualifiedName: "C", FilePath: "c.go"}
	if langC != "" {
		c.MetadataJSON = []byte(`{"language":"` + langC + `"}`)
	}
	for _, n := range []graphquery.Node{
		{ID: "A", Kind: graph.NodeFunction, Name: "A", QualifiedName: "A", FilePath: "a.go"},
		c,
	} {
		v.NodesByID[n.ID] = n
		v.NodesByName[n.Name] = append(v.NodesByName[n.Name], n)
		v.NodesByQualified[n.QualifiedName] = append(v.NodesByQualified[n.QualifiedName], n)
		v.NodesByPath[n.FilePath] = append(v.NodesByPath[n.FilePath], n)
	}
	e := graphquery.Edge{Kind: graph.EdgeCalls, SrcID: "C", DstID: "A"}
	v.EdgesBySrc["C"] = append(v.EdgesBySrc["C"], e)
	v.EdgesByDst["A"] = append(v.EdgesByDst["A"], e)
	v.EdgesByKind[graph.EdgeCalls] = append(v.EdgesByKind[graph.EdgeCalls], e)
	return v
}

// An all-Go call neighborhood is fully type-resolved: Resolved, not partial.
func TestEnrichGraphResolvedAllGo(t *testing.T) {
	gr, ok := EnrichGraph(IntentCallers, recallView(""), nil, []SymbolHit{{QualifiedName: "A", Path: "a.go"}})
	if !ok || gr == nil {
		t.Fatal("expected a call graph")
	}
	if !gr.Resolved || gr.RecallPartial {
		t.Fatalf("all-Go call graph: want resolved=true partial=false, got resolved=%v partial=%v", gr.Resolved, gr.RecallPartial)
	}
}

// A call edge touching a non-Go (tree-sitter) node is name-based: RecallPartial,
// not Resolved — an empty/partial result on such a language is not proof.
func TestEnrichGraphRecallPartialNonGo(t *testing.T) {
	gr, ok := EnrichGraph(IntentCallers, recallView("python"), nil, []SymbolHit{{QualifiedName: "A", Path: "a.go"}})
	if !ok || gr == nil {
		t.Fatal("expected a call graph")
	}
	if gr.Resolved || !gr.RecallPartial {
		t.Fatalf("non-Go caller: want resolved=false partial=true, got resolved=%v partial=%v", gr.Resolved, gr.RecallPartial)
	}
}

// A neighborhood with no call edges (architecture rollup / imports only) makes
// no resolved claim: Resolved=false, RecallPartial=false. Absence of a claim,
// not a negative one.
func TestEnrichGraphNoCallEdgesUnresolved(t *testing.T) {
	// symbol_lookup surfaces the has-method/field neighborhood, never call edges.
	v := recallView("")
	// Drop the call edge so the lookup lane finds a node but no calls.
	v.EdgesBySrc = map[string][]graphquery.Edge{}
	v.EdgesByDst = map[string][]graphquery.Edge{}
	v.EdgesByKind = map[graph.EdgeKind][]graphquery.Edge{}
	gr, ok := EnrichGraph(IntentSymbolLookup, v, nil, []SymbolHit{{QualifiedName: "A", Path: "a.go"}})
	if !ok || gr == nil {
		t.Fatal("expected the symbol node to surface")
	}
	if gr.Resolved || gr.RecallPartial {
		t.Fatalf("no call edges: want resolved=false partial=false, got resolved=%v partial=%v", gr.Resolved, gr.RecallPartial)
	}
}
