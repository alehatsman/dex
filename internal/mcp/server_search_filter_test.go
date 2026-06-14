// Copyright 2026 Aleh Atsman
//
// Regression test for #512: a languages/path_glob filter that rejects every
// ranked hit must surface a diagnostic hint, so an empty result from a
// typo'd filter is distinguishable from a genuine "query matched nothing".

package mcp

import (
	"strings"
	"testing"

	"github.com/alehatsman/dex/internal/store"
)

func mkHits(paths ...string) []store.Hit {
	hits := make([]store.Hit, len(paths))
	for i, p := range paths {
		hits[i] = store.Hit{Path: p}
	}
	return hits
}

func TestFilterMissHint(t *testing.T) {
	pre := mkHits("internal/mcp/server.go", "README.md", "cmd/dex/main.go")

	tests := []struct {
		name        string
		langs       []string
		exts        []string
		glob        string
		wantSubstrs []string
	}{
		{
			name:        "unrecognized language lists present extensions",
			langs:       []string{"klingon"},
			exts:        []string{"klingon"},
			wantSubstrs: []string{"languages filter", "klingon", "extensions present:", "go", "md", "dropped"},
		},
		{
			name:        "path_glob miss is named",
			glob:        "src/**",
			wantSubstrs: []string{"path_glob", "src/**", "matched none", "dropped"},
		},
		{
			name:        "both filters reported",
			langs:       []string{"rust"},
			exts:        []string{"rs"},
			glob:        "vendor/**",
			wantSubstrs: []string{"languages filter", "path_glob", "vendor/**"},
		},
		{
			name:        "no active filter yields no hint",
			wantSubstrs: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filterMissHint(tt.langs, tt.exts, tt.glob, pre)
			if tt.wantSubstrs == nil {
				if got != "" {
					t.Errorf("expected empty hint, got %q", got)
				}
				return
			}
			for _, sub := range tt.wantSubstrs {
				if !strings.Contains(got, sub) {
					t.Errorf("hint %q missing substring %q", got, sub)
				}
			}
		})
	}
}

// TestFilterHitsToEmptyIsDetectable locks the precondition the handler uses:
// an unrecognized language (treated as a raw extension) drops all hits, so
// the handler can detect filter-induced emptiness and call filterMissHint.
func TestFilterHitsToEmptyIsDetectable(t *testing.T) {
	pre := mkHits("a.go", "b.md", "c.ts")
	exts := langToExtensions([]string{"klingon"})
	got := filterHits(pre, exts, "", 10)
	if len(got) != 0 {
		t.Fatalf("expected unrecognized language to drop all hits, got %d", len(got))
	}
	glob := ""
	filteredToEmpty := len(got) == 0 && len(pre) > 0 && (len(exts) > 0 || glob != "")
	if !filteredToEmpty {
		t.Error("filteredToEmpty should be true when a filter drops a non-empty hit set")
	}
}
