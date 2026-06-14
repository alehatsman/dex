package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withCapturedStdout runs fn with os.Stdout redirected to a pipe and returns
// what was written.
func withCapturedStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	runErr := fn()
	w.Close()
	os.Stdout = orig
	out, _ := io.ReadAll(r)
	return string(out), runErr
}

// TestCmdReadLeadingPath covers #519: `dex read` must accept the optional
// leading [<path>] positional that every other verb takes, so
// `dex read <dir> <file>` works the same as `dex read <file>`.
func TestCmdReadLeadingPath(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "hello.txt")
	if err := os.WriteFile(file, []byte("line1\nline2\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	ctx := context.Background()

	t.Run("bare file", func(t *testing.T) {
		out, err := withCapturedStdout(t, func() error {
			return cmdRead(ctx, []string{file})
		})
		if err != nil {
			t.Fatalf("cmdRead(file) error: %v", err)
		}
		if !strings.Contains(out, "line1") {
			t.Errorf("output missing content: %q", out)
		}
	})

	t.Run("leading path positional", func(t *testing.T) {
		out, err := withCapturedStdout(t, func() error {
			return cmdRead(ctx, []string{dir, file})
		})
		if err != nil {
			t.Fatalf("cmdRead(dir, file) error: %v", err)
		}
		if !strings.Contains(out, "line1") {
			t.Errorf("output missing content: %q", out)
		}
	})

	t.Run("too many file args still errors", func(t *testing.T) {
		_, err := withCapturedStdout(t, func() error {
			return cmdRead(ctx, []string{dir, file, file})
		})
		if err == nil {
			t.Fatal("expected error for two file args, got nil")
		}
	})
}

func TestRangeText(t *testing.T) {
	lines := []string{"a", "b", "c", "d", "e"}
	cases := []struct {
		name       string
		start, end int
		want       string
	}{
		{"no range", 0, 0, "a\nb\nc\nd\ne"},
		{"start only", 3, 0, "c\nd\ne"},
		{"end only", 0, 2, "a\nb"},
		{"both", 2, 4, "b\nc\nd"},
		{"single line", 3, 3, "c"},
		{"end past eof clamps", 4, 99, "d\ne"},
		{"start past eof empty", 99, 0, ""},
		{"start beyond end empty", 4, 2, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := rangeText(lines, c.start, c.end); got != c.want {
				t.Errorf("rangeText(%d,%d) = %q, want %q", c.start, c.end, got, c.want)
			}
		})
	}
}
