package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestStatusDoesNotCloseCachedStore pins #514: the index_status scan must not
// close a store drawn from the shared per-project cache. Before the fix it
// called s.openStore (cached) and then Close()d it, poisoning the handle reused
// by the watcher and every query handler — leaving them with a persistent
// "sql: database is closed" until the server restarted.
func TestStatusDoesNotCloseCachedStore(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, filepath.Join(projDir, "main.go"),
		"package main\n\nfunc Greet(name string) string { return \"hi \" + name }\n")
	indexProject(t, projDir, cacheDir, srv.URL)

	// Locate the indexed project's DB the same way the status scan does.
	dbPath := findIndexDB(t, cacheDir)

	s := newServer(srv.URL, cacheDir)

	// Warm the shared cache the way the watcher / a prior query would.
	cached, err := s.openStore(dbPath)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	if _, err := cached.Stats(context.Background()); err != nil {
		t.Fatalf("cached store unhealthy before status: %v", err)
	}

	// Run the status handler — it scans every project under IndexDir.
	if _, _, err := s.status(context.Background(), nil, StatusInput{}); err != nil {
		t.Fatalf("status: %v", err)
	}

	// The shared handle must still be open afterwards.
	if _, err := cached.Stats(context.Background()); err != nil {
		t.Fatalf("cached store closed by status (#514 regression): %v", err)
	}
	// A fresh fetch from the cache must return a working handle too.
	again, err := s.openStore(dbPath)
	if err != nil {
		t.Fatalf("openStore after status: %v", err)
	}
	if _, err := again.Stats(context.Background()); err != nil {
		t.Fatalf("re-fetched cached store unusable after status: %v", err)
	}
}

func findIndexDB(t *testing.T, indexDir string) string {
	t.Helper()
	entries, err := os.ReadDir(indexDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(indexDir, e.Name(), "index.db")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Fatalf("no index.db found under %s", indexDir)
	return ""
}
