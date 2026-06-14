package mcp

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// TestGrepNotSearchThrottled pins #513: grep is a cheap deterministic local
// scan, so it must neither be blocked by the search-group budget nor ever
// return the empty "loop-blocked" shape that an agent reads as a genuine
// no-matches result. Its RE2 scan is already done by the time the loop
// detector runs, so blocking would save nothing and only lose results.
func TestGrepNotSearchThrottled(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()

	// One distinct, trigram-friendly token per line so each pattern below
	// genuinely matches a line in the file.
	var b strings.Builder
	b.WriteString("package main\n\n")
	for i := 0; i < 20; i++ {
		b.WriteString(fmt.Sprintf("var token%04d = %d\n", i, i))
	}
	writeFile(t, filepath.Join(projDir, "main.go"), b.String())
	root := indexProject(t, projDir, cacheDir, srv.URL)
	s := newServer(srv.URL, cacheDir)
	ctx := context.Background()

	// Distinct patterns (distinct fingerprints) past the search-group limit of
	// 10. Under the old search-class classification the 11th+ call would be
	// search-group-blocked; with the fix grep never consumes that budget.
	for i := 0; i < 14; i++ {
		out, err := s.SearchGrep(ctx, SearchGrepInput{
			ProjectRoot: root,
			Pattern:     fmt.Sprintf("token%04d", i),
		})
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if out.Status == "loop-blocked" {
			t.Fatalf("call %d: grep returned loop-blocked; cheap local scan must not be search-throttled", i)
		}
		if len(out.Matches) == 0 {
			t.Fatalf("call %d: matches suppressed (status=%s hint=%q); grep must always return its scan results", i, out.Status, out.Hint)
		}
	}

	// Repeating one identical pattern past the per-fingerprint block threshold
	// (6) must still return real results — at most trimmed, never the empty
	// loop-blocked payload.
	for i := 0; i < 8; i++ {
		out, err := s.SearchGrep(ctx, SearchGrepInput{ProjectRoot: root, Pattern: "token0000"})
		if err != nil {
			t.Fatalf("repeat %d: %v", i, err)
		}
		if out.Status == "loop-blocked" {
			t.Fatalf("repeat %d: grep returned loop-blocked on identical pattern; results must never be suppressed", i)
		}
		if len(out.Matches) == 0 {
			t.Fatalf("repeat %d: matches suppressed (status=%s hint=%q)", i, out.Status, out.Hint)
		}
	}
}
