package mcp

import (
	"testing"
)

func TestReorderByRecencyAndTask(t *testing.T) {
	seeds := []string{"a/foo.go"}
	session := map[string]struct{}{"b/bar.go": {}}

	// score legend:
	//   "a/foo.go" → seed (recency=2) + no task match → 2
	//   "b/bar.go" → session (recency=1) + no task match → 1
	//   "c/baz.go" → cold (recency=0) + task match ("baz") → 1
	//   "d/qux.go" → cold (recency=0) + no task match → 0
	paths := []string{"d/qux.go", "c/baz.go", "b/bar.go", "a/foo.go"}
	got := reorderByRecencyAndTask(paths, seeds, session, "baz feature")

	// Expected order: a/foo.go (2), b/bar.go (1) or c/baz.go (1), d/qux.go (0)
	if got[0] != "a/foo.go" {
		t.Errorf("expected a/foo.go first (seed), got %s", got[0])
	}
	if got[3] != "d/qux.go" {
		t.Errorf("expected d/qux.go last (cold, no task), got %s", got[3])
	}
	// b/bar.go and c/baz.go both score 1; original order within bucket preserved.
	if got[1] != "c/baz.go" || got[2] != "b/bar.go" {
		t.Errorf("mid-tier order: got %v, want [c/baz.go b/bar.go]", got[1:3])
	}
}

func TestReorderByRecencyAndTask_EmptySession(t *testing.T) {
	paths := []string{"x/main.go", "y/util.go"}
	got := reorderByRecencyAndTask(paths, nil, nil, "")
	if len(got) != 2 {
		t.Fatalf("expected 2 paths, got %d", len(got))
	}
	// No seeds, no session, no task → all score 0, original order preserved.
	if got[0] != "x/main.go" || got[1] != "y/util.go" {
		t.Errorf("unexpected order with no context: %v", got)
	}
}

func TestReorderByRecencyAndTask_SeedAlwaysFirst(t *testing.T) {
	seeds := []string{"cold/seed.go"}
	paths := []string{"hot/task_match.go", "cold/seed.go"}
	// seed scores 2, task-match cold scores 1 → seed wins even without task overlap.
	got := reorderByRecencyAndTask(paths, seeds, nil, "task_match")
	if got[0] != "cold/seed.go" {
		t.Errorf("seed should beat task-only match; got first=%s", got[0])
	}
}
