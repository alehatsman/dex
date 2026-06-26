package mcp

import (
	"context"
	"strings"
	"testing"
)

// TestTaskMapNoEmbedClient: when no embed client is wired the task path
// returns status=no-embed rather than crashing.
func TestTaskMapNoEmbedClient(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	_, cacheDir, root := recapProject(t, srv.URL) // reuse fixture builder

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
	projDir, cacheDir, root := recapProject(t, srv.URL)
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
	projDir, cacheDir, root := recapProject(t, srv.URL)
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
