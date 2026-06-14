// Copyright 2026 Aleh Atsman
//
// Regression test for #516: a Go tree with no go.mod silently produced an
// empty graph (darkening trace/impact/callers/path) with no warning. The
// extractor must surface an actionable warning when Go files are present
// but extraction is skipped — and stay silent for a non-Go tree.

package graph

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractGoWarnsOnMissingGoMod(t *testing.T) {
	root := t.TempDir()
	// Go file present, but no go.mod / go.work.
	if err := os.WriteFile(filepath.Join(root, "main.go"),
		[]byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := ExtractGo(context.Background(), root)
	if err != nil {
		t.Fatalf("ExtractGo: %v", err)
	}
	if res.Packages != 0 {
		t.Fatalf("packages: got %d, want 0 (no module)", res.Packages)
	}
	if len(res.Warnings) == 0 {
		t.Fatal("expected a warning about the missing go.mod, got none")
	}
	joined := strings.Join(res.Warnings, "\n")
	if !strings.Contains(joined, "go.mod") || !strings.Contains(joined, "go mod init") {
		t.Errorf("warning should name go.mod and the fix, got: %q", joined)
	}
}

func TestExtractGoSilentOnNonGoTree(t *testing.T) {
	root := t.TempDir()
	// Not a Go project at all — must produce no warning (no false alarm).
	if err := os.WriteFile(filepath.Join(root, "README.md"),
		[]byte("# docs\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := ExtractGo(context.Background(), root)
	if err != nil {
		t.Fatalf("ExtractGo: %v", err)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("non-Go tree should be silent, got warnings: %v", res.Warnings)
	}
}

func TestCountGoFilesSkipsHeavyDirs(t *testing.T) {
	root := t.TempDir()
	write := func(rel string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("package x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.go")
	write("pkg/b.go")
	write("vendor/dep/c.go")   // skipped
	write("node_modules/d.go") // skipped
	write(".git/hooks/e.go")   // skipped (dotdir)
	write("testdata/f.go")     // skipped
	write("notes.txt")         // not .go

	if got := countGoFiles(root); got != 2 {
		t.Errorf("countGoFiles = %d, want 2 (a.go, pkg/b.go)", got)
	}
}
