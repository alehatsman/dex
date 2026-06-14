package main

import (
	"encoding/json"
	"testing"

	"github.com/alehatsman/dex/internal/store"
)

// TestHitsToJSONExposesSortScore locks issue #509: the JSON `find` output must
// carry sort_score = the authoritative rank key (DisplayScore), so consumers
// have a single field that is monotonic with hit order — unlike raw `score`.
func TestHitsToJSONExposesSortScore(t *testing.T) {
	hits := []store.Hit{
		// rerank ran: RerankScore is the sort key, cosine Score is lower/unordered.
		{Path: "a.go", Score: 0.30, RRFScore: 0.7, RerankScore: 0.95},
		// local-rerank only: SortScore is the key.
		{Path: "b.go", Score: 0.80, RRFScore: 0.6, SortScore: 0.50},
		// semantic-only: falls back to cosine Score.
		{Path: "c.go", Score: 0.20},
	}
	out := hitsToJSON(hits)
	want := []float32{0.95, 0.50, 0.20}
	for i, w := range want {
		if out[i].SortScore != w {
			t.Errorf("hit %d (%s): sort_score = %v, want %v", i, out[i].Path, out[i].SortScore, w)
		}
		// score stays the raw cosine diagnostic, distinct from sort_score.
		if out[i].Score != hits[i].Score {
			t.Errorf("hit %d: score = %v, want raw cosine %v", i, out[i].Score, hits[i].Score)
		}
	}

	// The JSON tag must be sort_score and always present (no omitempty).
	b, err := json.Marshal(out[2]) // semantic-only hit, SortScore == 0.20
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["sort_score"]; !ok {
		t.Errorf("sort_score key missing from JSON: %s", b)
	}
}
