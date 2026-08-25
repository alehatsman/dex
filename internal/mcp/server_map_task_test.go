package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/store"
)

// TestTaskMapNoEmbedClient: when no embed client is wired the task path
// returns status=no-embed rather than crashing.
func TestTaskMapNoEmbedClient(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	_, cacheDir, root := indexedSvcProject(t, srv.URL)

	// Build a server WITHOUT an embed client.
	s := &Server{IndexDir: cacheDir}
	defer s.waitSessionWrites()
	_, out, err := s.taskMap(context.Background(), MapInput{Task: "add rate limiting", ProjectRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "no-embed" {
		t.Errorf("status = %q, want no-embed", out.Status)
	}
}

// TestTaskMapNoIndex: task path returns status=no-index on an unknown project.
func TestTaskMapNoIndex(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	s := newServer(srv.URL, cacheDir)
	defer s.waitSessionWrites()
	_, out, err := s.taskMap(context.Background(), MapInput{Task: "foo", ProjectRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "no-index" {
		t.Errorf("status = %q, want no-index", out.Status)
	}
}

// TestTaskMapReturnsBuckets: with a live index and embed server the task map
// runs and returns a non-error status with the expected output shape (Zoom=task,
// Map non-empty, L2Count ≥ 0). We can't assert exact L0/L1 membership without
// controlling the embedding server's cosine output, so this is a smoke test.
func TestTaskMapReturnsBuckets(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	projDir, cacheDir, root := indexedSvcProject(t, srv.URL)
	writeFile(t, projDir+"/util.go", "package util\n\nfunc Retry() {}\n")
	seedRecapFixture(t, projDir, cacheDir, srv.URL)

	s := newServer(srv.URL, cacheDir)
	defer s.waitSessionWrites()
	_, out, err := s.taskMap(context.Background(), MapInput{Task: "wire the dispatcher", ProjectRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "ok" {
		t.Fatalf("status = %q, want ok; hint: %s", out.Status, out.Hint)
	}
	if out.Zoom != "task" {
		t.Errorf("zoom = %q, want task", out.Zoom)
	}
	if out.Map == "" {
		t.Error("Map field empty")
	}
	if out.Task != "wire the dispatcher" {
		t.Errorf("Task = %q", out.Task)
	}
	// The digest header must be present.
	if !strings.Contains(out.Map, "Task-filtered read list") {
		t.Errorf("Map missing header:\n%s", out.Map)
	}
}

// TestTaskMapDispatchedFromVerb: mapVerb routes to taskMap when Task is set and
// the handler is a *Server.
func TestTaskMapDispatchedFromVerb(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	projDir, cacheDir, root := indexedSvcProject(t, srv.URL)
	seedRecapFixture(t, projDir, cacheDir, srv.URL)

	s := newServer(srv.URL, cacheDir)
	defer s.waitSessionWrites()
	_, out, err := mapVerb(context.Background(), s, nil, MapInput{Task: "wire the dispatcher", ProjectRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if out.Zoom != "task" {
		t.Errorf("zoom = %q, want task (dispatch should have reached taskMap)", out.Zoom)
	}
}

// indexedSvcProject builds a tiny indexed fixture project (one Go file) and
// returns its dirs + resolved root. Relocated from the deleted session recap
// tests (#195 S4) — the session-bounce map boost it feeds is internal and stays.
func indexedSvcProject(t *testing.T, srvURL string) (projDir, cacheDir, root string) {
	t.Helper()
	cacheDir = t.TempDir()
	projDir = t.TempDir()
	writeFile(t, projDir+"/svc.go", "package svc\n\nfunc Handle() {}\n")
	root = indexProject(t, projDir, cacheDir, srvURL)
	return projDir, cacheDir, root
}

// seedRecapFixture writes a small graph + session state (task + touched files)
// into the store. Relocated from the deleted session recap tests (#195 S4);
// SessionSetTask/SessionAddFile are the internal dedup machinery that stays.
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
