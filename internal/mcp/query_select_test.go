package mcp

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/alehatsman/dex/internal/graph"
	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/store"
)

func TestIsSelectorQuery(t *testing.T) {
	yes := []string{"pkg:store", "func:*Handler", "type:*Output", "file:internal/mcp/*.go", "kind:method", "pkg:mcp func:*Handler"}
	no := []string{"server.go:829", "internal/store", "(*Server).Run", "how are edits debounced", "pkg:", ":store", "pkg:store foo", "NewServer"}
	for _, s := range yes {
		if !isSelectorQuery(s) {
			t.Errorf("isSelectorQuery(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if isSelectorQuery(s) {
			t.Errorf("isSelectorQuery(%q) = true, want false", s)
		}
	}
}

func TestParseSelector(t *testing.T) {
	sel := parseSelector("pkg:store func:*Handler")
	if sel.Pkg != "%store%" {
		t.Errorf("pkg = %q, want %%store%%", sel.Pkg)
	}
	if sel.Name != "%Handler" {
		t.Errorf("name = %q, want %%Handler", sel.Name)
	}
	// func: expands to function+method kinds (sorted).
	if len(sel.Kinds) != 2 || sel.Kinds[0] != "function" || sel.Kinds[1] != "method" {
		t.Errorf("kinds = %v, want [function method]", sel.Kinds)
	}
	// bare name is exact (no wildcard, not substring).
	if got := parseSelector("func:NewServer").Name; got != "NewServer" {
		t.Errorf("bare func name = %q, want exact NewServer", got)
	}
}

func TestNormalizeKind(t *testing.T) {
	cases := []struct {
		val  string
		want []string
	}{
		{"func", []string{"function", "method"}}, // shorthand mirrors the func: field
		{"fn", []string{"function", "method"}},
		{"FUNC", []string{"function", "method"}}, // case-insensitive
		{"meth", []string{"method"}},
		{"iface", []string{"interface"}},
		{"function", []string{"function"}}, // exact stored kind passes through
		{"interface", []string{"interface"}},
		{"struct", []string{"struct"}},
	}
	for _, c := range cases {
		got := normalizeKind(c.val)
		if len(got) != len(c.want) {
			t.Errorf("normalizeKind(%q) = %v, want %v", c.val, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("normalizeKind(%q) = %v, want %v", c.val, got, c.want)
				break
			}
		}
	}
}

// TestParseSelectorKindAlias locks the #217 fix: `kind:func` must expand to the
// stored kinds (function+method), not pass the bare word `func` through — which
// matched nothing (stored kinds are function/method) and returned a silent empty.
func TestParseSelectorKindAlias(t *testing.T) {
	sel := parseSelector("kind:func")
	if len(sel.Kinds) != 2 || sel.Kinds[0] != "function" || sel.Kinds[1] != "method" {
		t.Errorf("kind:func -> %v, want [function method]", sel.Kinds)
	}
	// An exact stored kind is untouched.
	if got := parseSelector("kind:interface").Kinds; len(got) != 1 || got[0] != "interface" {
		t.Errorf("kind:interface -> %v, want [interface]", got)
	}
	// kind: unions with a func:/type: field's kinds (deduped, sorted).
	if got := parseSelector("func:Foo kind:func").Kinds; len(got) != 2 || got[0] != "function" || got[1] != "method" {
		t.Errorf("func:Foo kind:func -> %v, want [function method]", got)
	}
}

func TestGlobToLike(t *testing.T) {
	cases := []struct {
		glob      string
		substring bool
		want      string
	}{
		{"NewServer", false, "NewServer"},
		{"*Handler", false, "%Handler"},
		{"store", true, "%store%"},
		{"internal/*/x.go", true, "internal/%/x.go"},
		{"a_b", false, `a\_b`}, // literal underscore escaped
	}
	for _, c := range cases {
		if got := store.GlobToLike(c.glob, c.substring); got != c.want {
			t.Errorf("GlobToLike(%q,%v) = %q, want %q", c.glob, c.substring, got, c.want)
		}
	}
}

// selectFixture indexes a file and seeds a graph with a few named symbols so the
// selector query has something to enumerate.
func selectFixture(t *testing.T) (context.Context, *Server, string, func(QueryInput) QueryOutput) {
	t.Helper()
	srv := fakeEmbed(t, 16)
	t.Cleanup(srv.Close)
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	src := "package main\n\n" +
		"func pipeLeaf() int { return 7 }\n\n" +
		"func pipeMid() int { return pipeLeaf() + 1 }\n\n" +
		"func selectHandler() {}\n"
	writeFile(t, filepath.Join(projDir, "main.go"), src)
	root := indexProject(t, projDir, cacheDir, srv.URL)
	ctx := context.Background()
	seedSelectGraph(t, ctx, root, cacheDir)
	h := newServer(srv.URL, cacheDir)
	call := func(in QueryInput) QueryOutput {
		in.ProjectRoot = root
		_, out, err := queryVerb(ctx, h, &sdk.CallToolRequest{}, in)
		if err != nil {
			t.Fatalf("queryVerb(%q): %v", in.Input, err)
		}
		return out
	}
	return ctx, h, root, call
}

func seedSelectGraph(t *testing.T, ctx context.Context, root, cacheDir string) {
	t.Helper()
	p, err := proj.Resolve(root, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, p.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now()
	mk := func(name, kind string, start int) store.GraphNodeRow {
		return store.GraphNodeRow{
			ID: "m::main::" + kind + "::" + name, Kind: kind, Name: name, QualifiedName: name,
			PackagePath: "main", FilePath: "main.go", StartLine: start, EndLine: start + 1,
			MetadataJSON: []byte("{}"), ContentHash: "n-" + name,
		}
	}
	nodes := []store.GraphNodeRow{
		mk("pipeLeaf", string(graph.NodeFunction), 3),
		mk("pipeMid", string(graph.NodeFunction), 5),
		mk("selectHandler", string(graph.NodeFunction), 7),
	}
	if err := st.GraphUpsertNodes(ctx, nodes, now); err != nil {
		t.Fatal(err)
	}
	// one edge so pipeLeaf has a caller for the pipe-seed test.
	edges := []store.GraphEdgeRow{
		{ID: "e1", Kind: string(graph.EdgeCalls), SrcID: nodes[1].ID, DstID: nodes[0].ID,
			FilePath: "main.go", StartLine: 5, EndLine: 6, MetadataJSON: []byte("{}"), ContentHash: "c1"},
	}
	if err := st.GraphUpsertEdges(ctx, edges, now); err != nil {
		t.Fatal(err)
	}
}

// TestSelectorLaneStandalone: a `func:*` selector enumerates the fixture funcs,
// ranked by pagerank, on the select lane.
func TestSelectorLaneStandalone(t *testing.T) {
	_, _, _, call := selectFixture(t)
	out := call(QueryInput{Input: "func:pipe*"})
	if out.Status != "ok" {
		t.Fatalf("status = %q, want ok (caveat=%q)", out.Status, out.Trust.Caveat)
	}
	if out.Route.Detected != "selector" || out.Route.Lane != "select" {
		t.Fatalf("route = %+v, want detected=selector lane=select", out.Route)
	}
	if out.Result.Select == nil || out.Result.Select.Count != 2 {
		t.Fatalf("select result = %+v, want 2 (pipeLeaf,pipeMid)", out.Result.Select)
	}
	if out.Trust.Provenance != "name-based" {
		t.Errorf("provenance = %q, want name-based", out.Trust.Provenance)
	}
	// refs carry the same symbols for pipe threading.
	if len(out.Refs) != 2 {
		t.Fatalf("refs = %d, want 2", len(out.Refs))
	}
	// Deterministic order: ORDER BY pagerank DESC, in_degree DESC, name ASC.
	// Seeded nodes share pagerank=0 (centrality is a separate pass GraphUpsertNodes
	// doesn't run), so the name-ASC tiebreak decides — pipeLeaf before pipeMid.
	if out.Refs[0].ID != "pipeLeaf" {
		t.Errorf("refs[0] = %q, want pipeLeaf (name-ASC tiebreak)", out.Refs[0].ID)
	}
}

// TestSelectorKindFilter: type: excludes plain functions.
func TestSelectorKindFilter(t *testing.T) {
	_, _, _, call := selectFixture(t)
	out := call(QueryInput{Input: "type:*"})
	if out.Status != "not-found" {
		t.Fatalf("status = %q, want not-found (fixture has no types)", out.Status)
	}
}

// TestSelectorAsPipeSeed: `func:pipeLeaf | callers` composes — the selector seed
// feeds the callers transform, matching the manual two-step.
func TestSelectorAsPipeSeed(t *testing.T) {
	_, _, _, call := selectFixture(t)
	out := call(QueryInput{Input: "func:pipeLeaf | callers"})
	if out.Status != "ok" {
		t.Fatalf("status = %q, want ok (caveat=%q)", out.Status, out.Trust.Caveat)
	}
	if out.Route.Detected != "pipe" {
		t.Fatalf("detected = %q, want pipe", out.Route.Detected)
	}
	if out.Route.Stages[0] != "select" {
		t.Errorf("stages[0] = %q, want select", out.Route.Stages[0])
	}
	// pipeMid calls pipeLeaf, so it must appear among the callers.
	if !containsSuffix(refIDs(out.Refs), "pipeMid") {
		t.Errorf("expected pipeMid among callers, got %v", refIDs(out.Refs))
	}
}
