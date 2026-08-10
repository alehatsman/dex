package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/store"
)

// chunklessFixture builds a project where main.go is indexed (has chunks) but
// generated.ts exists on disk with NO chunks in the index — the exact #132
// condition (a file kept out of the chunk table but present in the working
// tree). Returns the server and the project root.
func chunklessFixture(t *testing.T) (*Server, string) {
	t.Helper()
	idxDir := t.TempDir()
	projDir := t.TempDir()

	writeFile(t, filepath.Join(projDir, "main.go"), "package main\nfunc Foo(){}\n")
	writeFile(t, filepath.Join(projDir, "generated.ts"), "export const NEEDLE_TOKEN = 42;\n")

	p, err := proj.Resolve(projDir, idxDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureCacheDir(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), p.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	// Only main.go gets chunks; generated.ts is deliberately absent from the index.
	if err := st.UpsertMany(context.Background(), []store.PendingChunk{
		{Path: "main.go", Kind: "fn", Name: "Foo", StartLine: 2, EndLine: 2, ContentSHA: "h1", Content: "func Foo(){}"},
	}, time.Now()); err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	_ = st.Close()

	return &Server{IndexDir: idxDir}, projDir
}

// TestFileTreeListsChunklessFile is the #132 regression: file_tree must list a
// file that is on disk but has no chunks in the index (shown with 0 chunks),
// instead of dropping it because it has no rows in the chunk table.
func TestFileTreeListsChunklessFile(t *testing.T) {
	s, root := chunklessFixture(t)
	_, out, err := s.searchTree(context.Background(), nil, SearchTreeInput{ProjectRoot: root, Depth: 1})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "ok" {
		t.Fatalf("status = %q, want ok (hint=%q)", out.Status, out.Hint)
	}
	byPath := map[string]int{}
	for _, e := range out.Entries {
		byPath[e.Path] = e.Chunks
	}
	if _, ok := byPath["generated.ts"]; !ok {
		t.Fatalf("chunkless generated.ts missing from file tree; entries=%v", out.Entries)
	}
	if byPath["generated.ts"] != 0 {
		t.Errorf("generated.ts chunks = %d, want 0", byPath["generated.ts"])
	}
	if byPath["main.go"] != 1 {
		t.Errorf("main.go chunks = %d, want 1 (index count must survive)", byPath["main.go"])
	}
	if out.TotalFiles != 2 {
		t.Errorf("TotalFiles = %d, want 2 (both working-tree files)", out.TotalFiles)
	}
}

// TestGrepFindsChunklessFile is the #132 regression for grep: a pattern living
// only in a chunkless on-disk file must still be found, because grep enumerates
// the working tree, not the chunk index.
func TestGrepFindsChunklessFile(t *testing.T) {
	s, root := chunklessFixture(t)
	_, out, err := s.searchGrep(context.Background(), nil, SearchGrepInput{
		ProjectRoot: root,
		Pattern:     "NEEDLE_TOKEN",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "ok" {
		t.Fatalf("status = %q, want ok (hint=%q)", out.Status, out.Hint)
	}
	found := false
	for _, m := range out.Matches {
		if m.Path == "generated.ts" {
			found = true
		}
	}
	if !found {
		t.Fatalf("NEEDLE_TOKEN not found in chunkless generated.ts; matches=%v", out.Matches)
	}
}

// TestWalkProjectFilesRespectsExcludes verifies the shared enumerator honors the
// ignore set (so grep/ls don't sweep build junk) while still returning normal
// source files.
func TestWalkProjectFilesRespectsExcludes(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "keep.go"), "package main\n")
	writeFile(t, filepath.Join(root, ".gitignore"), "dist/\n")
	if err := os.MkdirAll(filepath.Join(root, "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "dist", "bundle.js"), "junk\n")
	if err := os.MkdirAll(filepath.Join(root, "node_modules", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "node_modules", "x", "index.js"), "junk\n")

	got, err := walkProjectFiles(root, "")
	if err != nil {
		t.Fatal(err)
	}
	rels := map[string]bool{}
	for _, abs := range got {
		rel, _ := filepath.Rel(root, abs)
		rels[filepath.ToSlash(rel)] = true
	}
	if !rels["keep.go"] {
		t.Errorf("keep.go missing; got %v", rels)
	}
	if rels["dist/bundle.js"] {
		t.Errorf("gitignored dist/bundle.js leaked into enumeration")
	}
	if rels["node_modules/x/index.js"] {
		t.Errorf("node_modules leaked into enumeration")
	}
}
