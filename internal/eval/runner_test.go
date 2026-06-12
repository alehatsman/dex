package eval

import (
	"testing"

	"github.com/alehatsman/dex/internal/store"
)

func TestUniqueFilesExcludesAnchor(t *testing.T) {
	hits := []store.Hit{
		{Path: "anchor.go", Kind: "function_declaration"},
		{Path: "related.go", Kind: "window"},
	}
	// Blast-radius: the anchor is the given — it must not appear in the ranked
	// list (it would otherwise occupy a top-k slot and never count as relevant).
	if got := uniqueFiles(hits, 10, "anchor.go"); !eq(got, []string{"related.go"}) {
		t.Errorf("exclude anchor: got %v, want [related.go]", got)
	}
	if got := uniqueFiles(hits, 10, ""); len(got) != 2 {
		t.Errorf("no exclude: got %v, want 2 files", got)
	}
}

func TestUniqueFilesRespectsLimit(t *testing.T) {
	hits := []store.Hit{
		{Path: "a.go", Kind: "window"},
		{Path: "b.go", Kind: "window"},
		{Path: "c.go", Kind: "window"},
	}
	if got := uniqueFiles(hits, 2, ""); len(got) != 2 {
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
