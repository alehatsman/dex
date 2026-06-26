package mcp

import (
	"os"
	"path/filepath"
	"testing"
)

// TestImpactTestsToRun covers #654: impactTestsToRun returns the sibling tests
// of the blast-radius files (targets + displayed callers), deduped + sorted,
// and skips files that have no sibling test.
func TestImpactTestsToRun(t *testing.T) {
	root := t.TempDir()
	// foo.go + bar.go have sibling tests; baz.go does not.
	for _, f := range []string{"foo.go", "foo_test.go", "bar.go", "bar_test.go", "baz.go"} {
		if err := os.WriteFile(filepath.Join(root, f), []byte("package p\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	targets := []TargetMatch{{Path: "foo.go"}}
	nodes := []ImpactNode{
		{Path: "bar.go"},
		{Path: "foo.go"}, // dup of the target — must not double-count
		{Path: "baz.go"}, // no sibling test — contributes nothing
		{Path: ""},       // empty path — ignored
	}

	got := impactTestsToRun(root, targets, nodes)
	want := []string{"bar_test.go", "foo_test.go"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}

	// A target whose own test exists is included even with no callers.
	if only := impactTestsToRun(root, []TargetMatch{{Path: "bar.go"}}, nil); len(only) != 1 || only[0] != "bar_test.go" {
		t.Errorf("target-only: got %v, want [bar_test.go]", only)
	}
	// No impacted files → no tests.
	if none := impactTestsToRun(root, nil, nil); len(none) != 0 {
		t.Errorf("empty: got %v, want none", none)
	}
}
