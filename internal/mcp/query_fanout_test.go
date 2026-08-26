package mcp

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/store"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// stampProjectRoot records the project_root meta indexProject's direct
// internal/index.Run path leaves unset (only the real `dex index` CLI
// command stamps it, via store.SetProjectRoot in cmd/dex/main_index.go) —
// needed for proj.KnownRoots (the project_roots: ["all"] sentinel) to find
// a test fixture the same way it finds a real indexed project.
func stampProjectRoot(t *testing.T, cacheDir, root string) {
	t.Helper()
	p, err := proj.Resolve(root, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), p.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SetProjectRoot(context.Background(), root); err != nil {
		t.Fatal(err)
	}
}

// TestQueryFanout drives project_roots (#221) over two real indexed fixtures
// sharing one cache dir, plus a third root that was never indexed, proving:
// per-project isolation (each entry answers about its OWN file), request
// order (not completion order), and that one bad root degrades to a
// per-entry error instead of failing the whole call.
func TestQueryFanout(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()

	aDir := t.TempDir()
	writeFile(t, filepath.Join(aDir, "alpha.go"), "package main\n\nfunc Alpha() int { return 1 }\n")
	aRoot := indexProject(t, aDir, cacheDir, srv.URL)

	bDir := t.TempDir()
	writeFile(t, filepath.Join(bDir, "beta.go"), "package main\n\nfunc Beta() int { return 2 }\n")
	bRoot := indexProject(t, bDir, cacheDir, srv.URL)

	unindexed := t.TempDir()

	h := newServer(srv.URL, cacheDir)
	ctx := context.Background()

	_, out, err := h.query(ctx, &sdk.CallToolRequest{}, QueryInput{
		Input:        "alpha.go",
		ProjectRoots: []string{aRoot, unindexed, bRoot},
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(out.Fanout) != 3 {
		t.Fatalf("fanout len = %d, want 3: %+v", len(out.Fanout), out.Fanout)
	}

	// Request order preserved, not completion order.
	if out.Fanout[0].Root != aRoot || out.Fanout[1].Root != unindexed || out.Fanout[2].Root != bRoot {
		t.Fatalf("fanout order = %v", out.Fanout)
	}

	// Each indexed root answers about ITS OWN alpha.go — no cross-project bleed.
	if out.Fanout[0].Status != "ok" || out.Fanout[0].Result == nil || out.Fanout[0].Result.Result.Read == nil {
		t.Fatalf("root a: want ok read result, got %+v", out.Fanout[0])
	}
	// bRoot has no alpha.go, so its own read lane degrades to a
	// file-does-not-exist error — proving b resolved against ITS OWN project,
	// not silently returning a's alpha.go content.
	if out.Fanout[2].Result == nil || out.Fanout[2].Status == "ok" {
		t.Fatalf("root b: want a degraded (missing-file) status, got %+v", out.Fanout[2])
	}

	// The unindexed root degrades to its own status, not a hard failure of the
	// whole fan-out.
	if out.Fanout[1].Root != unindexed {
		t.Fatalf("unindexed entry root = %q", out.Fanout[1].Root)
	}
	if out.Fanout[1].Result == nil || out.Fanout[1].Result.Status == "" {
		t.Fatalf("unindexed root: want a degraded status, got %+v", out.Fanout[1])
	}
}

// TestQueryFanoutAllSentinel proves project_roots: ["all"] expands via the
// same discovery dex reindex --all uses (proj.KnownRoots), and that
// duplicates between the sentinel's expansion and an explicit root collapse
// to one entry.
func TestQueryFanoutAllSentinel(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()

	aDir := t.TempDir()
	writeFile(t, filepath.Join(aDir, "alpha.go"), "package main\n\nfunc Alpha() int { return 1 }\n")
	aRoot := indexProject(t, aDir, cacheDir, srv.URL)
	stampProjectRoot(t, cacheDir, aRoot)

	bDir := t.TempDir()
	writeFile(t, filepath.Join(bDir, "beta.go"), "package main\n\nfunc Beta() int { return 2 }\n")
	bRoot := indexProject(t, bDir, cacheDir, srv.URL)
	stampProjectRoot(t, cacheDir, bRoot)

	h := newServer(srv.URL, cacheDir)
	ctx := context.Background()

	_, out, err := h.query(ctx, &sdk.CallToolRequest{}, QueryInput{
		Input:        "",
		Kind:         "orient",
		ProjectRoots: []string{"all", aRoot},
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(out.Fanout) != 2 {
		t.Fatalf("fanout len = %d, want 2 (dedup aRoot against 'all'), got %+v", len(out.Fanout), out.Fanout)
	}
	got := map[string]bool{out.Fanout[0].Root: true, out.Fanout[1].Root: true}
	if !got[aRoot] || !got[bRoot] {
		t.Fatalf("fanout roots = %v, want {%s, %s}", out.Fanout, aRoot, bRoot)
	}
}
