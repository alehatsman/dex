// Copyright 2026 Aleh Atsman
//
// Regression test for #533: the "no exact symbol match" near-miss hint must
// only fire when the symbol lane actually whiffed (symbols[] empty). A query
// that is a substring of other symbol names but also has an exact def must
// not emit a hint contradicting the exact matches it returns.

package mcp

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

const nearMissHintPrefix = "no exact symbol match"

func TestSymbolNearMissSuppressedWhenExactMatchExists(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	// "Run" is an exact def AND a substring of RunBench / RunResult.
	writeFile(t, filepath.Join(projDir, "run.go"),
		"package main\n\nfunc Run() {}\nfunc RunBench() {}\nfunc RunResult() int { return 0 }\n")
	root := indexProject(t, projDir, cacheDir, srv.URL)
	s := newServer(srv.URL, cacheDir)

	_, out, err := s.ContextRouter(context.Background(), ContextInput{
		Question:    "Run",
		ProjectRoot: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Symbols) == 0 {
		t.Skip("symbol lane did not populate (routing/index); near-miss gate not exercised")
	}
	if strings.Contains(out.Hint, nearMissHintPrefix) {
		t.Errorf("near-miss hint must not fire when symbols[] has exact matches; got hint: %q", out.Hint)
	}
}

func TestSymbolNearMissStillFiresWhenNoExactMatch(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	// Only RunBench exists — a query for "Run" has no exact def but a
	// substring candidate, so the near-miss hint should still surface.
	writeFile(t, filepath.Join(projDir, "run.go"),
		"package main\n\nfunc RunBench() {}\n")
	root := indexProject(t, projDir, cacheDir, srv.URL)
	s := newServer(srv.URL, cacheDir)

	_, out, err := s.ContextRouter(context.Background(), ContextInput{
		Question:    "Run",
		ProjectRoot: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Symbols) != 0 {
		t.Skipf("unexpected exact symbol match (%d) — near-miss path not exercised", len(out.Symbols))
	}
	if !strings.Contains(out.Hint, nearMissHintPrefix) {
		t.Errorf("expected near-miss hint when no exact match exists; got hint: %q", out.Hint)
	}
}
