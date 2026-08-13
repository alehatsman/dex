package mcp

import "testing"

// An empty index (0 chunks) must report the config-shaped `index-empty` status,
// and it must win over the embed-failed / no-match branches — no query or
// embedder state can conjure a match from an empty index (#161).
func TestNoLaneHitsEmptyIndex(t *testing.T) {
	t.Run("empty index → index-empty", func(t *testing.T) {
		var out ContextOutput
		if !noLaneHits(false, false, true, &out) {
			t.Fatal("noLaneHits returned false on an empty index")
		}
		if out.Status != "index-empty" {
			t.Errorf("status = %q, want index-empty", out.Status)
		}
		if out.NextAction == "" {
			t.Error("index-empty should carry a next_action steering away from rephrasing")
		}
	})

	t.Run("empty index wins over embed-failed", func(t *testing.T) {
		var out ContextOutput
		noLaneHits(true, false, true, &out)
		if out.Status != "index-empty" {
			t.Errorf("status = %q, want index-empty to precede embed-failed", out.Status)
		}
	})

	t.Run("non-empty, no hits → not index-empty", func(t *testing.T) {
		var out ContextOutput
		noLaneHits(false, false, false, &out)
		if out.Status == "index-empty" {
			t.Error("non-empty index must not report index-empty")
		}
	})

	t.Run("hits present → false", func(t *testing.T) {
		out := ContextOutput{Symbols: []SymbolHit{{}}}
		if noLaneHits(false, false, true, &out) {
			t.Error("noLaneHits returned true despite a symbol hit")
		}
	})
}
