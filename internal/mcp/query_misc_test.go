package mcp

import (
	"os"
	"path/filepath"
	"testing"
)

// TestInferDepsTarget covers #504 (ported from the CLI's former `graph deps`
// subcommand by #849): a query(kind=deps) input is mapped to a file Path or a
// full Package by filesystem inference relative to the project root.
func TestInferDepsTarget(t *testing.T) {
	root := t.TempDir()
	// root/pkg/a.go (+ a_test.go), root/pkg/b.go, root/onlytest/x_test.go
	mustWrite(t, filepath.Join(root, "pkg", "a.go"), "package pkg\n")
	mustWrite(t, filepath.Join(root, "pkg", "a_test.go"), "package pkg\n")
	mustWrite(t, filepath.Join(root, "pkg", "b.go"), "package pkg\n")
	mustWrite(t, filepath.Join(root, "onlytest", "x_test.go"), "package onlytest\n")

	tests := []struct {
		name     string
		target   string
		wantFile string
		wantPkg  string
	}{
		{"existing file", "pkg/a.go", "pkg/a.go", ""},
		{"package dir prefers non-test file", "pkg", "pkg/a.go", ""},
		{"dir with only test files falls back to test", "onlytest", "onlytest/x_test.go", ""},
		{"non-existent path is a full import package", "github.com/foo/bar/internal/baz", "", "github.com/foo/bar/internal/baz"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file, pkg := inferDepsTarget(root, tt.target)
			if file != tt.wantFile || pkg != tt.wantPkg {
				t.Errorf("inferDepsTarget(%q) = (file=%q, pkg=%q), want (file=%q, pkg=%q)",
					tt.target, file, pkg, tt.wantFile, tt.wantPkg)
			}
		})
	}
}

func TestFirstGoFile(t *testing.T) {
	dir := t.TempDir()
	if got := firstGoFile(dir); got != "" {
		t.Errorf("empty dir: got %q, want \"\"", got)
	}
	mustWrite(t, filepath.Join(dir, "z_test.go"), "package x\n")
	if got := firstGoFile(dir); got != filepath.Join(dir, "z_test.go") {
		t.Errorf("test-only dir: got %q, want the test file", got)
	}
	mustWrite(t, filepath.Join(dir, "m.go"), "package x\n")
	if got := firstGoFile(dir); got != filepath.Join(dir, "m.go") {
		t.Errorf("mixed dir: got %q, want non-test m.go", got)
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}
