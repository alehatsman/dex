package mcp

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// buildGoFile writes a realistic Go file with n functions to projDir/name.
func buildGoFile(t *testing.T, projDir, name string, n int, extra string) {
	t.Helper()
	var sb strings.Builder
	sb.WriteString("package x\n\n")
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&sb, "// Func%d does something useful.\nfunc Func%d() int { return %d }\n\n", i, i, i)
	}
	if extra != "" {
		sb.WriteString(extra)
	}
	writeFile(t, projDir+"/"+name, sb.String())
}

// TestDeltaReRead verifies that re-reading a large file after a small edit returns
// status="delta" with a compact unified diff instead of the full file.
func TestDeltaReRead(t *testing.T) {
	projDir := t.TempDir()
	cacheDir := t.TempDir()

	// Write a ~60-function file so delta is well below the 60% threshold.
	buildGoFile(t, projDir, "a.go", 60, "")

	s := &Server{IndexDir: cacheDir}
	ctx := context.Background()

	// First read — establishes the content baseline.
	_, out1, err := s.summarize(ctx, nil, SummarizeInput{
		Path:        "a.go",
		ProjectRoot: projDir,
		Mode:        "lines:1-200",
	})
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if out1.Status != "ok" {
		t.Fatalf("first read status = %q, want ok; hint: %s", out1.Status, out1.Hint)
	}
	etag1 := out1.Etag

	// Add a single function to the end.
	buildGoFile(t, projDir, "a.go", 60, "func Extra() int { return 999 }\n")

	// Second read with old etag — file changed, delta should be returned.
	_, out2, err := s.summarize(ctx, nil, SummarizeInput{
		Path:        "a.go",
		ProjectRoot: projDir,
		Mode:        "lines:1-200",
		Etag:        etag1,
	})
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if out2.Status != "delta" {
		t.Fatalf("second read status = %q, want delta; hint=%s content=%q", out2.Status, out2.Hint, out2.Content)
	}
	if !strings.Contains(out2.Content, "+func Extra()") {
		t.Errorf("delta content missing +func Extra(): %q", out2.Content)
	}
	if out2.Etag == etag1 {
		t.Errorf("etag should update after delta delivery")
	}
}

// TestDeltaReRead_Unchanged verifies the existing unchanged fast-path is unaffected.
func TestDeltaReRead_Unchanged(t *testing.T) {
	projDir := t.TempDir()
	cacheDir := t.TempDir()

	buildGoFile(t, projDir, "a.go", 20, "")

	s := &Server{IndexDir: cacheDir}
	ctx := context.Background()

	_, out1, _ := s.summarize(ctx, nil, SummarizeInput{
		Path: "a.go", ProjectRoot: projDir, Mode: "lines:1-80",
	})
	if out1.Status != "ok" {
		t.Fatalf("first read: status=%q", out1.Status)
	}

	_, out2, err := s.summarize(ctx, nil, SummarizeInput{
		Path: "a.go", ProjectRoot: projDir, Mode: "lines:1-80", Etag: out1.Etag,
	})
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if out2.Status != "unchanged" {
		t.Errorf("want status=unchanged, got %q", out2.Status)
	}
}

// TestDeltaReRead_LargeChange falls through to full content when diff exceeds threshold.
func TestDeltaReRead_LargeChange(t *testing.T) {
	projDir := t.TempDir()
	cacheDir := t.TempDir()

	// Write 50 lines of original content.
	var orig strings.Builder
	for i := 0; i < 50; i++ {
		orig.WriteString("// original line content that will be wholesale replaced\n")
	}
	writeFile(t, projDir+"/a.go", orig.String())

	s := &Server{IndexDir: cacheDir}
	ctx := context.Background()

	_, out1, _ := s.summarize(ctx, nil, SummarizeInput{
		Path: "a.go", ProjectRoot: projDir, Mode: "lines:1-60",
	})
	if out1.Status != "ok" {
		t.Fatalf("first read: %q", out1.Status)
	}

	// Replace all content.
	var repl strings.Builder
	for i := 0; i < 50; i++ {
		repl.WriteString("// completely different replacement content here now\n")
	}
	writeFile(t, projDir+"/a.go", repl.String())

	_, out2, err := s.summarize(ctx, nil, SummarizeInput{
		Path: "a.go", ProjectRoot: projDir, Mode: "lines:1-60", Etag: out1.Etag,
	})
	if err != nil {
		t.Fatalf("large change read: %v", err)
	}
	// Delta not worth — should return full content.
	if out2.Status != "ok" {
		t.Errorf("want status=ok (delta not worth it), got %q", out2.Status)
	}
}

// TestDeltaReRead_SubsequentDelta verifies a third read after a delta also works.
func TestDeltaReRead_SubsequentDelta(t *testing.T) {
	projDir := t.TempDir()
	cacheDir := t.TempDir()

	buildGoFile(t, projDir, "a.go", 60, "")

	s := &Server{IndexDir: cacheDir}
	ctx := context.Background()

	_, out1, _ := s.summarize(ctx, nil, SummarizeInput{
		Path: "a.go", ProjectRoot: projDir, Mode: "lines:1-200",
	})
	if out1.Status != "ok" {
		t.Fatalf("first read: %q", out1.Status)
	}

	// Second edit — add one line.
	buildGoFile(t, projDir, "a.go", 60, "func Extra1() {}\n")
	_, out2, _ := s.summarize(ctx, nil, SummarizeInput{
		Path: "a.go", ProjectRoot: projDir, Mode: "lines:1-200", Etag: out1.Etag,
	})
	if out2.Status != "delta" {
		t.Fatalf("second read: want delta, got %q", out2.Status)
	}

	// Third edit — add another line.
	buildGoFile(t, projDir, "a.go", 60, "func Extra1() {}\nfunc Extra2() {}\n")
	_, out3, err := s.summarize(ctx, nil, SummarizeInput{
		Path: "a.go", ProjectRoot: projDir, Mode: "lines:1-200", Etag: out2.Etag,
	})
	if err != nil {
		t.Fatalf("third read: %v", err)
	}
	if out3.Status != "delta" {
		t.Fatalf("third read: want delta, got %q; content=%q", out3.Status, out3.Content)
	}
	if !strings.Contains(out3.Content, "+func Extra2()") {
		t.Errorf("third delta missing Extra2: %q", out3.Content)
	}
}
