package mcp

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/alehatsman/dex/internal/graph"
	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/store"
)

func TestParseSinceSeed(t *testing.T) {
	cases := []struct {
		in      string
		wantRef string
		wantOK  bool
	}{
		{"since:HEAD~3", "HEAD~3", true},
		{"diff:HEAD~3", "HEAD~3", true},
		{"SINCE:HEAD~3", "HEAD~3", true},
		{"since:working", "working", true},
		{"since:", "", true},
		{"since:v1.0.0..v2.0.0", "v1.0.0..v2.0.0", true},
		{"pkg:store", "", false},
		{"server.go:829", "", false},
		{"(*Server).Run", "", false},
	}
	for _, c := range cases {
		ref, ok := parseSinceSeed(c.in)
		if ok != c.wantOK || ref != c.wantRef {
			t.Errorf("parseSinceSeed(%q) = (%q,%v), want (%q,%v)", c.in, ref, ok, c.wantRef, c.wantOK)
		}
	}
}

// sinceFixture commits v1 (Greet + Caller), then v2 modifying Greet's body, and
// returns a query caller against the indexed project — the diff between the two
// commits is what since: seeds resolve against.
func sinceFixture(t *testing.T) (context.Context, string, func(QueryInput) QueryOutput) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	srv := fakeEmbed(t, 16)
	t.Cleanup(srv.Close)
	cacheDir := t.TempDir()
	projDir := t.TempDir()

	gitRun(t, projDir, "init", "-q")
	writeFile(t, filepath.Join(projDir, "greet.go"),
		"package main\n\nfunc Greet(name string) string { return \"hi \" + name }\n\nfunc Caller() string { return Greet(\"x\") }\n")
	gitRun(t, projDir, "add", ".")
	gitRun(t, projDir, "commit", "-q", "-m", "v1")
	writeFile(t, filepath.Join(projDir, "greet.go"),
		"package main\n\nfunc Greet(name string) string { return \"hello \" + name }\n\nfunc Caller() string { return Greet(\"x\") }\n")
	gitRun(t, projDir, "add", ".")
	gitRun(t, projDir, "commit", "-q", "-m", "v2")

	root := indexProject(t, projDir, cacheDir, srv.URL)
	ctx := context.Background()
	seedSinceGraph(t, ctx, root, cacheDir)
	h := newServer(srv.URL, cacheDir)
	call := func(in QueryInput) QueryOutput {
		in.ProjectRoot = root
		_, out, err := queryVerb(ctx, h, &sdk.CallToolRequest{}, in)
		if err != nil {
			t.Fatalf("queryVerb(%q): %v", in.Input, err)
		}
		return out
	}
	return ctx, root, call
}

// seedSinceGraph seeds a Caller-calls-Greet edge so `since:… | callers` has a
// real graph to walk. resolveHunkSymbols resolves hunk lines to bare chunk
// names (Greet, not main.Greet), so the graph nodes below match that shape —
// mirroring seedSelectGraph.
func seedSinceGraph(t *testing.T, ctx context.Context, root, cacheDir string) {
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
	mk := func(name string, start int) store.GraphNodeRow {
		return store.GraphNodeRow{
			ID: "m::main::function::" + name, Kind: string(graph.NodeFunction), Name: name, QualifiedName: name,
			PackagePath: "main", FilePath: "greet.go", StartLine: start, EndLine: start + 1,
			MetadataJSON: []byte("{}"), ContentHash: "n-" + name,
		}
	}
	nodes := []store.GraphNodeRow{mk("Greet", 3), mk("Caller", 5)}
	if err := st.GraphUpsertNodes(ctx, nodes, now); err != nil {
		t.Fatal(err)
	}
	edges := []store.GraphEdgeRow{
		{ID: "e1", Kind: string(graph.EdgeCalls), SrcID: nodes[1].ID, DstID: nodes[0].ID,
			FilePath: "greet.go", StartLine: 5, EndLine: 6, MetadataJSON: []byte("{}"), ContentHash: "c1"},
	}
	if err := st.GraphUpsertEdges(ctx, edges, now); err != nil {
		t.Fatal(err)
	}
}

// TestSinceLaneStandalone: `since:HEAD~1` resolves the symbols touched between
// the two committed revisions — Greet's modified body.
func TestSinceLaneStandalone(t *testing.T) {
	_, _, call := sinceFixture(t)
	out := call(QueryInput{Input: "since:HEAD~1"})
	if out.Status != "ok" {
		t.Fatalf("status = %q, want ok (caveat=%q)", out.Status, out.Trust.Caveat)
	}
	if out.Route.Detected != "since" || out.Route.Lane != "since" {
		t.Fatalf("route = %+v, want detected=since lane=since", out.Route)
	}
	if out.Result.Since == nil || out.Result.Since.Count != 1 || out.Result.Since.Symbols[0].ID != "Greet" {
		t.Fatalf("since result = %+v, want 1 symbol (Greet)", out.Result.Since)
	}
	if out.Trust.Provenance != "name-based" {
		t.Errorf("provenance = %q, want name-based", out.Trust.Provenance)
	}
	if len(out.Refs) != 1 || out.Refs[0].ID != "Greet" {
		t.Fatalf("refs = %v, want [Greet]", refIDs(out.Refs))
	}
}

// TestDiffAliasMatchesSince: `diff:` is a bare alias for `since:` — same ref,
// same result.
func TestDiffAliasMatchesSince(t *testing.T) {
	_, _, call := sinceFixture(t)
	out := call(QueryInput{Input: "diff:HEAD~1"})
	if out.Status != "ok" || out.Result.Since == nil || out.Result.Since.Count != 1 {
		t.Fatalf("diff: alias = %+v, want ok with 1 symbol", out)
	}
}

// TestSinceAsPipeSeed: `since:HEAD~1 | callers` composes — the since seed feeds
// the callers transform, matching #219's success criterion (same symbol set
// review_diff's blast radius would compute, fed through the existing pipe).
func TestSinceAsPipeSeed(t *testing.T) {
	_, _, call := sinceFixture(t)
	out := call(QueryInput{Input: "since:HEAD~1 | callers"})
	if out.Status != "ok" {
		t.Fatalf("status = %q, want ok (caveat=%q)", out.Status, out.Trust.Caveat)
	}
	if out.Route.Detected != "pipe" {
		t.Fatalf("detected = %q, want pipe", out.Route.Detected)
	}
	if out.Route.Stages[0] != "since" {
		t.Errorf("stages[0] = %q, want since", out.Route.Stages[0])
	}
	if !containsSuffix(refIDs(out.Refs), "Caller") {
		t.Errorf("expected Caller among callers of Greet, got %v", refIDs(out.Refs))
	}
}

// TestSinceLaneNoChanges: an empty range (no diff) is an honest not-found, not
// an error.
func TestSinceLaneNoChanges(t *testing.T) {
	_, _, call := sinceFixture(t)
	out := call(QueryInput{Input: "since:HEAD..HEAD"})
	if out.Status != "not-found" {
		t.Fatalf("status = %q, want not-found (no changes in an empty range)", out.Status)
	}
}
