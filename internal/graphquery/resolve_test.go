package graphquery

import (
	"reflect"
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

// TestResolveCallTargetsPackageFilterTail is the regression for #583: the
// `package` filter must accept a path suffix (the short package name an agent
// types) — not only the full import path — mirroring the "pkg.Foo" convention.
// Repro shape: two Get funcs, in .../config and .../mode.
func TestResolveCallTargetsPackageFilterTail(t *testing.T) {
	cfg := Node{
		ID: "g1", Kind: graph.NodeFunction, Name: "Get",
		QualifiedName: "Get", PackagePath: "github.com/gotify/server/v2/config",
	}
	mode := Node{
		ID: "g2", Kind: graph.NodeFunction, Name: "Get",
		QualifiedName: "Get", PackagePath: "github.com/gotify/server/v2/mode",
	}
	v := resolveView(cfg, mode)

	onlyCfg := func(t *testing.T, filter string) {
		t.Helper()
		got := ResolveCallTargets(v, "Get", filter)
		if len(got) != 1 || got[0].ID != cfg.ID {
			t.Errorf("ResolveCallTargets(Get, %q) = %v, want only the config.Get node", filter, got)
		}
	}
	// Tail segment, multi-segment suffix, and the full path all select config.
	onlyCfg(t, "config")
	onlyCfg(t, "v2/config")
	onlyCfg(t, "github.com/gotify/server/v2/config")
	// The other package still selectable by its tail.
	if got := ResolveCallTargets(v, "Get", "mode"); len(got) != 1 || got[0].ID != mode.ID {
		t.Errorf(`ResolveCallTargets(Get, "mode") = %v, want only the mode.Get node`, got)
	}
	// Suffix must respect the "/" boundary — a partial segment matches nothing.
	if got := ResolveCallTargets(v, "Get", "fig"); len(got) != 0 {
		t.Errorf(`ResolveCallTargets(Get, "fig") = %v, want no match (suffix must start on a path segment)`, got)
	}
	// No filter returns both interpretations.
	if got := ResolveCallTargets(v, "Get", ""); len(got) != 2 {
		t.Errorf("ResolveCallTargets(Get, \"\") = %d matches, want 2", len(got))
	}
}

// TestPkgFilterCandidates covers the not-found-hint helper: when the bare name
// resolves in several packages, it lists them sorted so the caller can tell the
// agent the filter was too narrow rather than the symbol missing (#583).
func TestPkgFilterCandidates(t *testing.T) {
	cfg := Node{ID: "g1", Kind: graph.NodeFunction, Name: "Get", QualifiedName: "Get", PackagePath: "github.com/gotify/server/v2/config"}
	mode := Node{ID: "g2", Kind: graph.NodeFunction, Name: "Get", QualifiedName: "Get", PackagePath: "github.com/gotify/server/v2/mode"}
	v := resolveView(cfg, mode)

	got := PkgFilterCandidates(v, "Get")
	want := []string{"github.com/gotify/server/v2/config", "github.com/gotify/server/v2/mode"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PkgFilterCandidates(Get) = %v, want %v", got, want)
	}
	if got := PkgFilterCandidates(v, "Nonexistent"); len(got) != 0 {
		t.Errorf("PkgFilterCandidates(Nonexistent) = %v, want empty", got)
	}
}
