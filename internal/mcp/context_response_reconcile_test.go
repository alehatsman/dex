// Copyright 2026 Aleh Atsman
//
// Regression test for #532: next_action must not point at a different
// "primary site" than the synthesized answer it ships with.

package mcp

import (
	"strings"
	"testing"
)

func TestReconcileNextActionWithAnswer(t *testing.T) {
	reads := []SuggestedRead{
		{Path: "internal/cache/cache.go", StartLine: 10, EndLine: 40}, // raw-score winner
		{Path: "internal/store/store.go", StartLine: 100, EndLine: 160},
	}

	t.Run("rewrites when answer leads with a different read", func(t *testing.T) {
		out := &ContextOutput{
			Answer:         "The write path lives in internal/store/store.go:120 where commits flush.",
			NextAction:     "Read internal/cache/cache.go lines 10-40 first — this is the primary site.",
			SuggestedReads: reads,
		}
		reconcileNextActionWithAnswer(out)
		if !strings.Contains(out.NextAction, "internal/store/store.go") {
			t.Errorf("next_action should be re-pointed at the answer's lead, got: %q", out.NextAction)
		}
		if strings.Contains(out.NextAction, "primary site.") {
			t.Errorf("stale directive should have been replaced, got: %q", out.NextAction)
		}
	})

	t.Run("no-op when already consistent", func(t *testing.T) {
		na := "Read internal/cache/cache.go lines 10-40 first — this is the primary site."
		out := &ContextOutput{
			Answer:         "Caching is handled in internal/cache/cache.go.",
			NextAction:     na,
			SuggestedReads: reads,
		}
		reconcileNextActionWithAnswer(out)
		if out.NextAction != na {
			t.Errorf("consistent next_action should be untouched, got: %q", out.NextAction)
		}
	})

	t.Run("no-op for graph directive that names no read", func(t *testing.T) {
		na := "Follow graph.edges from markDirty to see the watcher fan-out."
		out := &ContextOutput{
			Answer:         "The write path lives in internal/store/store.go.",
			NextAction:     na,
			SuggestedReads: reads,
		}
		reconcileNextActionWithAnswer(out)
		if out.NextAction != na {
			t.Errorf("graph directive should be untouched, got: %q", out.NextAction)
		}
	})

	t.Run("no-op when answer leads with a non-suggested file", func(t *testing.T) {
		na := "Read internal/cache/cache.go lines 10-40 first — this is the primary site."
		out := &ContextOutput{
			Answer:         "It is wired in internal/mcp/server.go (not a suggested read).",
			NextAction:     na,
			SuggestedReads: reads,
		}
		reconcileNextActionWithAnswer(out)
		if out.NextAction != na {
			t.Errorf("ungrounded lead should not rewrite next_action, got: %q", out.NextAction)
		}
	})

	t.Run("no-op when there is no answer", func(t *testing.T) {
		na := "Read internal/cache/cache.go lines 10-40 first — this is the primary site."
		out := &ContextOutput{NextAction: na, SuggestedReads: reads}
		reconcileNextActionWithAnswer(out)
		if out.NextAction != na {
			t.Errorf("without an answer, next_action is authoritative, got: %q", out.NextAction)
		}
	})
}

func TestFirstAnswerLeadPath(t *testing.T) {
	tests := []struct {
		answer string
		want   string
	}{
		{"The logic is in internal/store/store.go:120.", "internal/store/store.go"},
		{"See a/b/c.go then d/e/f.go.", "a/b/c.go"},
		{"No path here at all.", ""},
		{"Bare watch.go has no slash.", ""},
	}
	for _, tt := range tests {
		if got := firstAnswerLeadPath(tt.answer); got != tt.want {
			t.Errorf("firstAnswerLeadPath(%q) = %q, want %q", tt.answer, got, tt.want)
		}
	}
}
