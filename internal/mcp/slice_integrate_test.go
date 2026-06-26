package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSummarizeSliceHead verifies head:N extraction via the Summarize path.
func TestSummarizeSliceHead(t *testing.T) {
	projDir := t.TempDir()
	// 10-line file
	buildGoFile(t, projDir, "a.go", 10, "")

	s := &Server{IndexDir: t.TempDir()}
	_, out, err := s.summarize(context.Background(), nil, SummarizeInput{
		Path:        "a.go",
		ProjectRoot: projDir,
		Slice:       "head:3",
	})
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if out.Status != "ok" {
		t.Fatalf("status=%q hint=%s", out.Status, out.Hint)
	}
	lines := strings.Split(strings.TrimRight(out.Content, "\n"), "\n")
	if len(lines) != 3 {
		t.Errorf("head:3 got %d lines, want 3; content=%q", len(lines), out.Content)
	}
	if out.Hint == "" {
		t.Error("expected non-empty hint from head:3")
	}
}

// TestSummarizeSliceTail verifies tail:N extraction.
func TestSummarizeSliceTail(t *testing.T) {
	projDir := t.TempDir()
	buildGoFile(t, projDir, "a.go", 10, "")

	s := &Server{IndexDir: t.TempDir()}
	_, out, err := s.summarize(context.Background(), nil, SummarizeInput{
		Path:        "a.go",
		ProjectRoot: projDir,
		Slice:       "tail:2",
	})
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if out.Status != "ok" {
		t.Fatalf("status=%q hint=%s", out.Status, out.Hint)
	}
	lines := strings.Split(strings.TrimRight(out.Content, "\n"), "\n")
	if len(lines) != 2 {
		t.Errorf("tail:2 got %d lines, want 2; content=%q", len(lines), out.Content)
	}
}

// TestSummarizeSliceSearch verifies search:PATTERN extraction.
func TestSummarizeSliceSearch(t *testing.T) {
	projDir := t.TempDir()
	content := "package main\n\nfunc hello() {}\n\nfunc world() {}\n"
	if err := os.WriteFile(filepath.Join(projDir, "main.go"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	s := &Server{IndexDir: t.TempDir()}
	_, out, err := s.summarize(context.Background(), nil, SummarizeInput{
		Path:        "main.go",
		ProjectRoot: projDir,
		Slice:       "search:hello",
	})
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if out.Status != "ok" {
		t.Fatalf("status=%q hint=%s", out.Status, out.Hint)
	}
	if !strings.Contains(out.Content, "hello") {
		t.Errorf("search:hello: expected 'hello' in output, got %q", out.Content)
	}
}

// TestSummarizeSliceSearchNoMatch verifies that a search with no matches
// returns status=ok with empty content and a descriptive hint.
func TestSummarizeSliceSearchNoMatch(t *testing.T) {
	projDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projDir, "a.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := &Server{IndexDir: t.TempDir()}
	_, out, err := s.summarize(context.Background(), nil, SummarizeInput{
		Path:        "a.go",
		ProjectRoot: projDir,
		Slice:       "search:zzznomatch",
	})
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if out.Status != "ok" {
		t.Fatalf("status=%q, want ok", out.Status)
	}
	if out.Content != "" {
		t.Errorf("no-match search: expected empty content, got %q", out.Content)
	}
	if !strings.Contains(out.Hint, "no matches") {
		t.Errorf("no-match search: hint %q should mention 'no matches'", out.Hint)
	}
}

// TestSummarizeSliceBadSpec verifies that an unknown slice spec is rejected.
func TestSummarizeSliceBadSpec(t *testing.T) {
	projDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projDir, "a.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := &Server{IndexDir: t.TempDir()}
	_, out, err := s.summarize(context.Background(), nil, SummarizeInput{
		Path:        "a.go",
		ProjectRoot: projDir,
		Slice:       "bogus:stuff",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Status != "error" {
		t.Errorf("bad spec: status=%q, want error", out.Status)
	}
}

// TestSummarizeSliceWithHandle verifies that slice composes with a file handle:
// the handle resolves the range first, then slice extracts within it.
func TestSummarizeSliceWithHandle(t *testing.T) {
	projDir := t.TempDir()
	buildGoFile(t, projDir, "a.go", 20, "")

	s := &Server{IndexDir: t.TempDir()}
	h := EncodeHandle("a.go", 5, 15) // handle pins lines 5-15
	_, out, err := s.summarize(context.Background(), nil, SummarizeInput{
		Handle:      h,
		ProjectRoot: projDir,
		Slice:       "head:3", // first 3 lines of the handle's range
	})
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if out.Status != "ok" {
		t.Fatalf("status=%q hint=%s", out.Status, out.Hint)
	}
	lines := strings.Split(strings.TrimRight(out.Content, "\n"), "\n")
	if len(lines) != 3 {
		t.Errorf("slice+handle: got %d lines, want 3; content=%q", len(lines), out.Content)
	}
}

// TestSummarizeCCRHashNotFound verifies the not-found response for an absent CCR hash.
func TestSummarizeCCRHashNotFound(t *testing.T) {
	s := &Server{CCRDir: t.TempDir()} // empty dir → no blobs
	_, out, err := s.summarize(context.Background(), nil, SummarizeInput{
		CCRHash: "deadbeef12345678",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Status != "not-found" {
		t.Errorf("CCR not-found: status=%q, want not-found", out.Status)
	}
}

// TestSummarizeCCRHashFound verifies retrieval of a CCR blob from disk.
func TestSummarizeCCRHashFound(t *testing.T) {
	ccrDir := t.TempDir()
	hash := "abcdef1234567890"
	blob := "line one\nline two\nline three\n"
	if err := os.WriteFile(filepath.Join(ccrDir, hash+".log"), []byte(blob), 0o644); err != nil {
		t.Fatal(err)
	}

	s := &Server{CCRDir: ccrDir}
	_, out, err := s.summarize(context.Background(), nil, SummarizeInput{
		CCRHash: hash,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Status != "ok" {
		t.Fatalf("CCR found: status=%q hint=%s", out.Status, out.Hint)
	}
	if out.Content != blob {
		t.Errorf("CCR content=%q, want %q", out.Content, blob)
	}
}

// TestSummarizeCCRWithSlice verifies that slice applies to a CCR blob.
func TestSummarizeCCRWithSlice(t *testing.T) {
	ccrDir := t.TempDir()
	hash := "abcdef1234567890"
	blob := "alpha\nbeta\ngamma\ndelta\n"
	if err := os.WriteFile(filepath.Join(ccrDir, hash+".log"), []byte(blob), 0o644); err != nil {
		t.Fatal(err)
	}

	s := &Server{CCRDir: ccrDir}
	_, out, err := s.summarize(context.Background(), nil, SummarizeInput{
		CCRHash: hash,
		Slice:   "search:beta",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Status != "ok" {
		t.Fatalf("CCR+slice: status=%q hint=%s", out.Status, out.Hint)
	}
	if !strings.Contains(out.Content, "beta") {
		t.Errorf("CCR+slice: expected 'beta' in output, got %q", out.Content)
	}
}
