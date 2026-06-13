package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/store"
)

// seedRecapFixture builds an index DB, upserts a couple of graph nodes for
// fixed files (recap's symbol skeleton comes from the graph, not from a live
// re-parse), and records those files plus a task on the session. recap groups
// symbols by the graph's FilePath, so the session path and the node FilePath
// must agree — they are set equal here on purpose.
func seedRecapFixture(t *testing.T, projDir, cacheDir, srvURL string) {
	t.Helper()
	ctx := context.Background()
	p, err := proj.Resolve(projDir, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, p.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	nodes := []store.GraphNodeRow{
		{ID: "svc.Handle", Kind: "function", Name: "Handle", QualifiedName: "svc.Handle", PackagePath: "svc", FilePath: "svc.go", StartLine: 3, EndLine: 5},
		{ID: "svc.Dispatch", Kind: "function", Name: "Dispatch", QualifiedName: "svc.Dispatch", PackagePath: "svc", FilePath: "svc.go", StartLine: 7, EndLine: 8},
		{ID: "util.Retry", Kind: "function", Name: "Retry", QualifiedName: "util.Retry", PackagePath: "util", FilePath: "util.go", StartLine: 1, EndLine: 9},
	}
	if err := st.GraphUpsertNodes(ctx, nodes, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := st.SessionSetTask(ctx, "wire the dispatcher"); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"svc.go", "util.go"} {
		if err := st.SessionAddFile(ctx, f, "read"); err != nil {
			t.Fatalf("SessionAddFile(%s): %v", f, err)
		}
	}
}

func recapProject(t *testing.T, srvURL string) (projDir, cacheDir, root string) {
	t.Helper()
	cacheDir = t.TempDir()
	projDir = t.TempDir()
	writeFile(t, projDir+"/svc.go", "package svc\n\nfunc Handle() {}\n")
	root = indexProject(t, projDir, cacheDir, srvURL)
	return projDir, cacheDir, root
}

// TestSessionRecapRendersSkeleton: recap delivers each working-set file's path
// and the qualified symbols it defines inline (the re-orientation digest #346),
// not a list of calls to re-run.
func TestSessionRecapRendersSkeleton(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	projDir, cacheDir, root := recapProject(t, srv.URL)
	seedRecapFixture(t, projDir, cacheDir, srv.URL)

	s := newServer(srv.URL, cacheDir)
	defer s.waitSessionWrites()
	_, out, err := s.session(context.Background(), nil, SessionInput{Action: "recap", ProjectRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "ok" {
		t.Fatalf("status = %q, want ok; hint: %s", out.Status, out.Hint)
	}
	for _, want := range []string{"svc.go", "util.go", "svc.Handle", "svc.Dispatch", "util.Retry", "wire the dispatcher"} {
		if !strings.Contains(out.Content, want) {
			t.Errorf("recap content missing %q\n%s", want, out.Content)
		}
	}
	if out.FileCount != 2 {
		t.Errorf("FileCount = %d, want 2", out.FileCount)
	}
	if out.UsedTokens <= 0 {
		t.Errorf("UsedTokens = %d, want > 0 (the digest spent budget)", out.UsedTokens)
	}
}

// TestSessionRecapBudgetTruncates: a budget too small for the whole working set
// drops files cheapest-first-packed and says so — the honest truncation the
// reorient lane gates.
func TestSessionRecapBudgetTruncates(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	projDir, cacheDir, root := recapProject(t, srv.URL)
	seedRecapFixture(t, projDir, cacheDir, srv.URL)

	s := newServer(srv.URL, cacheDir)
	defer s.waitSessionWrites()
	// Budget=1 cannot fit any file's entry, so the working set is fully omitted.
	_, out, err := s.session(context.Background(), nil, SessionInput{Action: "recap", ProjectRoot: root, Budget: 1})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "ok" {
		t.Fatalf("status = %q, want ok; hint: %s", out.Status, out.Hint)
	}
	if out.FileCount != 0 {
		t.Errorf("FileCount = %d, want 0 under a 1-token budget", out.FileCount)
	}
	if !strings.Contains(out.Content, "omitted to fit") {
		t.Errorf("recap content did not report budget truncation\n%s", out.Content)
	}
	if strings.Contains(out.Content, "svc.Handle") {
		t.Errorf("recap leaked an omitted file's symbols under a 1-token budget\n%s", out.Content)
	}
}

// TestSessionRecapNoSession: recap with no active session is a clean no-op, not
// an error.
func TestSessionRecapNoSession(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	_, cacheDir, root := recapProject(t, srv.URL)

	s := newServer(srv.URL, cacheDir)
	defer s.waitSessionWrites()
	_, out, err := s.session(context.Background(), nil, SessionInput{Action: "recap", ProjectRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "ok" {
		t.Fatalf("status = %q, want ok; hint: %s", out.Status, out.Hint)
	}
	if !strings.Contains(out.Hint, "no active session") {
		t.Errorf("hint = %q, want a no-active-session note", out.Hint)
	}
}
