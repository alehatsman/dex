package mcp

import (
	"strings"
	"testing"

	"github.com/alehatsman/dex/internal/graph"
	"github.com/alehatsman/dex/internal/graphquery"
)

// twoGetView mirrors the #583 repro: a bare name (Get) defined in two packages.
func twoGetView() *graphquery.View {
	v := &graphquery.View{
		NodesByID:        map[string]graphquery.Node{},
		NodesByName:      map[string][]graphquery.Node{},
		NodesByQualified: map[string][]graphquery.Node{},
		NodesByPath:      map[string][]graphquery.Node{},
		EdgesBySrc:       map[string][]graphquery.Edge{},
		EdgesByDst:       map[string][]graphquery.Edge{},
		EdgesByKind:      map[graph.EdgeKind][]graphquery.Edge{},
	}
	for _, n := range []graphquery.Node{
		{ID: "g1", Kind: graph.NodeFunction, Name: "Get", QualifiedName: "Get", PackagePath: "github.com/gotify/server/v2/config"},
		{ID: "g2", Kind: graph.NodeFunction, Name: "Get", QualifiedName: "Get", PackagePath: "github.com/gotify/server/v2/mode"},
	} {
		v.NodesByID[n.ID] = n
		v.NodesByName[n.Name] = append(v.NodesByName[n.Name], n)
		v.NodesByQualified[n.QualifiedName] = append(v.NodesByQualified[n.QualifiedName], n)
	}
	return v
}

// TestNotFoundHintNamesCandidatePackages is the #583 regression for the
// misleading hint: when a package filter excludes every match but the name
// exists elsewhere, the hint must name the real packages instead of telling the
// agent to mangle the symbol form.
func TestNotFoundHintNamesCandidatePackages(t *testing.T) {
	v := twoGetView()

	// Filter that excludes all matches but the name resolves in two packages.
	got := notFoundHint(v, "Get", "nonexistent")
	for _, want := range []string{"config", "mode", `name="Get"`} {
		if !strings.Contains(got, want) {
			t.Errorf("hint %q should mention %q", got, want)
		}
	}
	if strings.Contains(got, "receiver-qualified") {
		t.Errorf("filtered-out hint should not push the receiver-qualified spelling fix: %q", got)
	}

	// No package filter → the generic spelling hint, not the package list.
	generic := notFoundHint(v, "Missing", "")
	if !strings.Contains(generic, "receiver-qualified") {
		t.Errorf("unfiltered miss should give the generic spelling hint, got %q", generic)
	}
}
