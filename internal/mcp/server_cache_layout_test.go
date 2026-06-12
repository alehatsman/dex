package mcp

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/store"
)

// seedSession opens (creating if absent) the store for projDir/cacheDir and
// records paths as session-stable files. The session must exist (SessionSetTask
// creates it) before SessionAddFile can write file rows.
func seedSession(t *testing.T, projDir, cacheDir string, paths []string) {
	t.Helper()
	ctx := context.Background()
	p, err := proj.Resolve(projDir, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureCacheDir(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, p.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SessionSetTask(ctx, "test task"); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if err := st.SessionAddFile(ctx, path, "read"); err != nil {
			t.Fatalf("SessionAddFile(%s): %v", path, err)
		}
	}
}

// TestCacheLayoutStableFirst locks the stable_first reordering: when some
// paths are session-stable (recorded in the session) and others are fresh,
// stable files appear before fresh ones in the batch output regardless of
// the caller's input order. StablePrefixTokens is non-zero.
func TestCacheLayoutStableFirst(t *testing.T) {
	projDir := t.TempDir()
	cacheDir := t.TempDir()

	// Three files. auth.go is session-stable; math.go and log.go are fresh.
	writeFile(t, filepath.Join(projDir, "auth.go"),
		"package x\n\n// Authenticate validates a token.\nfunc Authenticate(tok string) error { return nil }\n")
	writeFile(t, filepath.Join(projDir, "math.go"),
		"package x\n\nfunc Add(a, b int) int { return a + b }\n")
	writeFile(t, filepath.Join(projDir, "log.go"),
		"package x\n\nfunc Log(msg string) { println(msg) }\n")
	writeIndexAll(t, projDir)

	// Mark auth.go as session-stable.
	seedSession(t, projDir, cacheDir, []string{"auth.go"})

	s := &Server{IndexDir: cacheDir}
	defer s.waitSessionWrites() // drain background store writes before TempDir cleanup
	ctx := context.Background()

	// Input order: math.go (fresh), auth.go (stable), log.go (fresh).
	// With stable_first, auth.go must move to position 0.
	_, out, err := s.summarize(ctx, nil, SummarizeInput{
		Paths:       []string{"math.go", "auth.go", "log.go"},
		ProjectRoot: projDir,
		Mode:        "lines:1-1",
	})
	if err != nil {
		t.Fatalf("summarizeBatch: %v", err)
	}
	if out.Status != "ok" {
		t.Fatalf("status = %q, want ok; hint: %s", out.Status, out.Hint)
	}

	// auth.go must be the first resolved path.
	if len(out.Paths) == 0 {
		t.Fatal("no resolved paths returned")
	}
	if out.Paths[0] != "auth.go" {
		t.Errorf("Paths[0] = %q, want auth.go (stable file should be first)", out.Paths[0])
	}

	// Stable prefix token count must be positive.
	if out.StablePrefixTokens <= 0 {
		t.Errorf("StablePrefixTokens = %d, want > 0", out.StablePrefixTokens)
	}

	// auth.go section must appear before math.go in the content string.
	authIdx := strings.Index(out.Content, "auth.go")
	mathIdx := strings.Index(out.Content, "math.go")
	if authIdx < 0 || mathIdx < 0 {
		t.Fatalf("content missing expected paths (auth=%d math=%d)", authIdx, mathIdx)
	}
	if authIdx > mathIdx {
		t.Errorf("auth.go (stable) appears after math.go (fresh) in content — reordering did not fire")
	}
}

// TestCacheLayoutRecencyPreservesOrder locks that CacheLayout="recency" on the
// input suppresses stable_first reordering and returns files in caller order.
// No session is seeded — the test verifies the layout override path, not the
// stability classifier.
func TestCacheLayoutRecencyPreservesOrder(t *testing.T) {
	projDir := t.TempDir()
	cacheDir := t.TempDir()

	writeFile(t, filepath.Join(projDir, "b.go"), "package x\nfunc B() {}\n")
	writeFile(t, filepath.Join(projDir, "a.go"), "package x\nfunc A() {}\n")
	writeIndexAll(t, projDir)
	// No session: all files are fresh, but CacheLayout=recency must still keep
	// b.go first (the caller's order) and emit StablePrefixTokens=0.

	s := &Server{IndexDir: cacheDir}
	defer s.waitSessionWrites() // drain background store writes before TempDir cleanup
	ctx := context.Background()

	_, out, err := s.summarize(ctx, nil, SummarizeInput{
		Paths:       []string{"b.go", "a.go"},
		ProjectRoot: projDir,
		Mode:        "lines:1-1",
		CacheLayout: "recency",
	})
	if err != nil {
		t.Fatalf("summarizeBatch: %v", err)
	}
	if out.Status != "ok" {
		t.Fatalf("status = %q, want ok; hint: %s", out.Status, out.Hint)
	}
	if len(out.Paths) < 2 {
		t.Fatalf("expected 2 paths, got %d", len(out.Paths))
	}
	if out.Paths[0] != "b.go" {
		t.Errorf("Paths[0] = %q, want b.go (recency must preserve caller order)", out.Paths[0])
	}
	if out.StablePrefixTokens != 0 {
		t.Errorf("StablePrefixTokens = %d, want 0 for recency layout", out.StablePrefixTokens)
	}
}

// TestCacheLayoutNoSession locks the no-op path: when no session exists,
// stable_first is effectively a no-op and the original input order is
// preserved (all files are treated as fresh).
func TestCacheLayoutNoSession(t *testing.T) {
	projDir := t.TempDir()
	cacheDir := t.TempDir()

	writeFile(t, filepath.Join(projDir, "a.go"), "package x\nfunc A() {}\n")
	writeFile(t, filepath.Join(projDir, "b.go"), "package x\nfunc B() {}\n")
	writeIndexAll(t, projDir)
	// No seedSession call — store exists but no session.

	s := &Server{IndexDir: cacheDir}
	defer s.waitSessionWrites() // drain background store writes before TempDir cleanup
	ctx := context.Background()

	_, out, err := s.summarize(ctx, nil, SummarizeInput{
		Paths:       []string{"a.go", "b.go"},
		ProjectRoot: projDir,
		Mode:        "lines:1-1",
	})
	if err != nil {
		t.Fatalf("summarizeBatch: %v", err)
	}
	if out.Status != "ok" {
		t.Fatalf("status = %q, want ok; hint: %s", out.Status, out.Hint)
	}
	// No stable files → prefix is zero.
	if out.StablePrefixTokens != 0 {
		t.Errorf("StablePrefixTokens = %d, want 0 (no session)", out.StablePrefixTokens)
	}
	// Original order preserved.
	if len(out.Paths) >= 2 && out.Paths[0] != "a.go" {
		t.Errorf("Paths[0] = %q, want a.go (no-session must preserve order)", out.Paths[0])
	}
}
