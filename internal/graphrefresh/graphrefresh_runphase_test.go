package graphrefresh

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/store"
)

// makeGoProject creates a minimal Go project in dir for RunPhase to index.
func makeGoProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/testpkg\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := `package testpkg

func Foo() {}

func Bar() {
	Foo()
}
`
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRunPhaseIndexesGoProject(t *testing.T) {
	ctx := context.Background()
	root := makeGoProject(t)

	cacheDir := t.TempDir()
	p := &proj.Project{
		Root:     root,
		ID:       "test",
		CacheDir: cacheDir,
		DBPath:   filepath.Join(cacheDir, "index.db"),
	}

	st, err := store.Open(ctx, p.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	stats, err := RunPhase(ctx, p, st, false, slog.Default())
	if err != nil {
		t.Fatalf("RunPhase: %v", err)
	}
	if stats == nil {
		t.Fatal("RunPhase returned nil stats")
	}

	// The project has 2 functions (Foo, Bar) → at least 2 nodes.
	n, _, err := st.GraphStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n < 2 {
		t.Errorf("graph has %d nodes after RunPhase, want >=2", n)
	}
}

// TestRunPhaseIdempotent: running twice on the same project should not error.
func TestRunPhaseIdempotent(t *testing.T) {
	ctx := context.Background()
	root := makeGoProject(t)

	cacheDir := t.TempDir()
	p := &proj.Project{
		Root:     root,
		ID:       "test",
		CacheDir: cacheDir,
		DBPath:   filepath.Join(cacheDir, "index.db"),
	}

	st, err := store.Open(ctx, p.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	for i := range 2 {
		if _, err := RunPhase(ctx, p, st, false, slog.Default()); err != nil {
			t.Fatalf("RunPhase pass %d: %v", i+1, err)
		}
	}

	// Stats should be stable.
	n1, _, _ := st.GraphStats(ctx)
	RunPhase(ctx, p, st, false, slog.Default()) //nolint:errcheck
	n2, _, _ := st.GraphStats(ctx)
	if n1 != n2 {
		t.Errorf("node count changed on third pass: %d → %d", n1, n2)
	}
}
