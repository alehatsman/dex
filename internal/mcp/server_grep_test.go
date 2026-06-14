package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestSearchGrepNotFound asserts that an explicit but non-existent subdir
// path fails loud (status "not-found") instead of silently walking the whole
// project root and returning a misleading "no-matches" (issue #73).
func TestSearchGrepNotFound(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	s := newServer(srv.URL, t.TempDir())

	projDir := t.TempDir()
	_, out, err := s.searchGrep(context.Background(), nil, SearchGrepInput{
		Pattern:     "foo",
		Path:        "does/not/exist",
		ProjectRoot: projDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "not-found" {
		t.Errorf("status = %q, want not-found", out.Status)
	}
}

// TestSearchGrepFilePath asserts that an explicit *file* path is accepted and
// scoped to exactly that file, rather than rejected with a lying "does not
// exist" message — grep used to require a directory (issue #534).
func TestSearchGrepFilePath(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	s := newServer(srv.URL, t.TempDir())

	projDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projDir, "target.go"), []byte("package p\n// needle here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projDir, "other.go"), []byte("package p\n// needle here too\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, out, err := s.searchGrep(context.Background(), nil, SearchGrepInput{
		Pattern:     "needle",
		Path:        "target.go",
		ProjectRoot: projDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "ok" {
		t.Fatalf("status = %q, want ok", out.Status)
	}
	if out.Total != 1 {
		t.Fatalf("total = %d, want 1 (scan scoped to the single file)", out.Total)
	}
	if len(out.Matches) != 1 || out.Matches[0].Path != "target.go" {
		t.Errorf("matches = %+v, want one in target.go", out.Matches)
	}
}

// TestSearchGrepFilePathExtExcluded asserts that a single-file scope with an
// ext filter that excludes the file yields no-matches, never a whole-repo walk.
func TestSearchGrepFilePathExtExcluded(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	s := newServer(srv.URL, t.TempDir())

	projDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projDir, "target.go"), []byte("// needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, out, err := s.searchGrep(context.Background(), nil, SearchGrepInput{
		Pattern:     "needle",
		Path:        "target.go",
		Ext:         "py",
		ProjectRoot: projDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "no-matches" {
		t.Errorf("status = %q, want no-matches", out.Status)
	}
}
