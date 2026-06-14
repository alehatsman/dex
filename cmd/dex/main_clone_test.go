package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/alehatsman/dex/internal/store"
)

func readProjectRoot(t *testing.T, ctx context.Context, dbPath string) string {
	t.Helper()
	st, err := store.OpenWith(ctx, dbPath, storeOpts())
	if err != nil {
		t.Fatalf("open %s: %v", dbPath, err)
	}
	defer st.Close()
	root, err := st.ProjectRoot(ctx)
	if err != nil {
		t.Fatalf("ProjectRoot(%s): %v", dbPath, err)
	}
	return root
}

// TestCloneDoesNotMutateSourceProjectRoot locks issue #517: cloning an index
// must retag only the destination's project_root and never touch the source.
// The bug was that copyFile hard-linked index.db when src and dst shared a
// filesystem, so the two indexes were the same inode — retagging dst's
// project_root silently overwrote src's. This drives cmdClone's two real
// steps (copyFile + retagProjectRoot) against a store-backed source.
func TestCloneDoesNotMutateSourceProjectRoot(t *testing.T) {
	ctx := context.Background()

	const srcRoot = "/orig/source/project"
	const dstRoot = "/new/destination/project"

	srcDB := filepath.Join(t.TempDir(), "index.db")
	st, err := store.OpenWith(ctx, srcDB, storeOpts())
	if err != nil {
		t.Fatalf("open source store: %v", err)
	}
	if err := st.SetProjectRoot(ctx, srcRoot); err != nil {
		t.Fatalf("set source project_root: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close source store: %v", err)
	}

	dstDB := filepath.Join(t.TempDir(), "index.db")

	// Mirror cmdClone: copy the DB, then retag the destination only.
	if err := copyFile(srcDB, dstDB); err != nil {
		t.Fatalf("copyFile: %v", err)
	}
	if err := retagProjectRoot(ctx, dstDB, dstRoot); err != nil {
		t.Fatalf("retagProjectRoot: %v", err)
	}

	if got := readProjectRoot(t, ctx, srcDB); got != srcRoot {
		t.Errorf("source project_root = %q, want %q — clone mutated the source (#517)", got, srcRoot)
	}
	if got := readProjectRoot(t, ctx, dstDB); got != dstRoot {
		t.Errorf("destination project_root = %q, want %q", got, dstRoot)
	}

	// The copy must be a distinct file: a shared inode is the root cause.
	si, err := os.Stat(srcDB)
	if err != nil {
		t.Fatalf("stat src: %v", err)
	}
	di, err := os.Stat(dstDB)
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	if os.SameFile(si, di) {
		t.Error("clone hard-linked the index — src and dst share an inode; writes to one corrupt the other")
	}
}
