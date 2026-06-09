package eval

import (
	"testing"

	"github.com/alehatsman/dex/internal/chunk"
	"github.com/alehatsman/dex/internal/store"
)

func TestUniqueFilesFiltersGitAndSummaries(t *testing.T) {
	hits := []store.Hit{
		{Path: "git:abcd1234", Kind: chunk.KindGitCommit},
		{Path: "a.go", Kind: "window"},
		{Path: "b.go", Kind: chunk.KindFileSummary},
		{Path: "a.go", Kind: chunk.KindChunkSummary}, // dup file, ignored anyway
		{Path: "c.go", Kind: "function_declaration"},
	}

	// Default: git_commit + summary chunks dropped → only code files.
	got := uniqueFiles(hits, 10, false)
	want := []string{"a.go", "c.go"}
	if !eq(got, want) {
		t.Errorf("keepSummaries=false: got %v, want %v", got, want)
	}

	// keepSummaries: summary chunks retained (b.go via file_summary), but
	// git_commit is still dropped unconditionally.
	got = uniqueFiles(hits, 10, true)
	want = []string{"a.go", "b.go", "c.go"}
	if !eq(got, want) {
		t.Errorf("keepSummaries=true: got %v, want %v", got, want)
	}
}

func TestUniqueFilesRespectsLimit(t *testing.T) {
	hits := []store.Hit{
		{Path: "a.go", Kind: "window"},
		{Path: "b.go", Kind: "window"},
		{Path: "c.go", Kind: "window"},
	}
	if got := uniqueFiles(hits, 2, false); len(got) != 2 {
		t.Errorf("limit=2: got %d files, want 2 (%v)", len(got), got)
	}
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
