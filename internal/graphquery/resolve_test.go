package graphquery

import (
	"testing"

	"github.com/alehatsman/dex/internal/graph"
)

func resolveView(nodes ...Node) *View {
	v := &View{
		NodesByID:        map[string]Node{},
		NodesByName:      map[string][]Node{},
		NodesByQualified: map[string][]Node{},
		NodesByPath:      map[string][]Node{},
		EdgesBySrc:       map[string][]Edge{},
		EdgesByDst:       map[string][]Edge{},
		EdgesByKind:      map[graph.EdgeKind][]Edge{},
	}
	for _, n := range nodes {
		v.NodesByID[n.ID] = n
		v.NodesByName[n.Name] = append(v.NodesByName[n.Name], n)
		v.NodesByQualified[n.QualifiedName] = append(v.NodesByQualified[n.QualifiedName], n)
	}
	return v
}

// TestResolveCallTargetsAcceptsTypeMethod is the regression for #571: the
// "Type.Method" form (Indexer.Run) is what an agent reaches for first, but it
// fell through every resolution step. All four equivalent spellings must now
// resolve to the same method node.
func TestResolveCallTargetsAcceptsTypeMethod(t *testing.T) {
	ptr := Node{
		ID: "m1", Kind: graph.NodeFunction, Name: "Run",
		QualifiedName: "(*Indexer).Run", PackagePath: "internal/index",
	}
	v := resolveView(ptr)
	for _, form := range []string{"Indexer.Run", "(*Indexer).Run", "index.Run", "Run"} {
		got := ResolveCallTargets(v, form, "")
		if len(got) != 1 || got[0].ID != ptr.ID {
			t.Errorf("ResolveCallTargets(%q) = %v, want the (*Indexer).Run node", form, got)
		}
	}

	// Value-receiver methods ("(T).M") must resolve via Type.Method too.
	val := Node{
		ID: "m2", Kind: graph.NodeFunction, Name: "ModelName",
		QualifiedName: "(Embedder).ModelName", PackagePath: "internal/embed",
	}
	vv := resolveView(val)
	if got := ResolveCallTargets(vv, "Embedder.ModelName", ""); len(got) != 1 || got[0].ID != val.ID {
		t.Errorf("ResolveCallTargets(Embedder.ModelName) = %v, want the value-receiver method", got)
	}
}

// TestResolveCallTargetsTypeMethodNoFalsePositive guards the receiver match
// from binding a like-named method on a different type.
func TestResolveCallTargetsTypeMethodNoFalsePositive(t *testing.T) {
	other := Node{
		ID: "m1", Kind: graph.NodeFunction, Name: "Run",
		QualifiedName: "(*Server).Run", PackagePath: "internal/mcp",
	}
	v := resolveView(other)
	if got := ResolveCallTargets(v, "Indexer.Run", ""); len(got) != 0 {
		t.Errorf("Indexer.Run must not match (*Server).Run, got %v", got)
	}
}
