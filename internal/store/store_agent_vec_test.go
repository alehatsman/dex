package store_test

import (
	"context"
	"testing"
	"time"
)

// TestAgentQueryVec covers the findings-bus vector recall path (#180):
// similarity ranking, the minSim floor, and the category filter.
func TestAgentQueryVec(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	// Two orthogonal findings + one status message in a different direction.
	post := func(cat, body string, vec []float32) int64 {
		id, err := s.AgentPostVec(ctx, "peer-1", "", cat, body, vec)
		if err != nil {
			t.Fatalf("post %q: %v", body, err)
		}
		return id
	}
	aID := post("finding", "assemble bounds output to the tool-result cap", []float32{1, 0, 0, 0})
	post("finding", "workspace resolver follows subpath exports", []float32{0, 1, 0, 0})
	post("status", "ci is green", []float32{1, 0, 0, 0}) // same direction as A, but wrong category

	// Query pointing almost exactly at A.
	got, err := s.AgentQueryVec(ctx, []float32{0.98, 0.02, 0, 0}, "finding", 5, 0.5)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 finding above floor, got %d: %+v", len(got), got)
	}
	if got[0].ID != aID {
		t.Errorf("top hit id = %d, want %d (assemble finding)", got[0].ID, aID)
	}
	if got[0].Category != "finding" {
		t.Errorf("category filter leaked: got %q", got[0].Category)
	}

	// Empty category = any: the status message (same vector direction as A) now
	// also clears the floor.
	any, err := s.AgentQueryVec(ctx, []float32{0.98, 0.02, 0, 0}, "", 5, 0.5)
	if err != nil {
		t.Fatalf("query any: %v", err)
	}
	if len(any) != 2 {
		t.Fatalf("want 2 hits across categories, got %d", len(any))
	}
}

// TestAgentQueryVecEmptyInputs asserts the no-op guards: empty query vector or
// no stored vectors return nil (not an error) so the caller can fall back.
func TestAgentQueryVecEmptyInputs(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	got, err := s.AgentQueryVec(ctx, nil, "finding", 5, 0.2)
	if err != nil || got != nil {
		t.Fatalf("empty vec: got=%v err=%v, want nil,nil", got, err)
	}
	// A post with no vector leaves the vec table empty → still nil, no error.
	if _, err := s.AgentPost(ctx, "peer-1", "", "finding", "unembedded note"); err != nil {
		t.Fatalf("post: %v", err)
	}
	got, err = s.AgentQueryVec(ctx, []float32{1, 0, 0, 0}, "finding", 5, 0.2)
	if err != nil || got != nil {
		t.Fatalf("no vectors: got=%v err=%v, want nil,nil", got, err)
	}
}

// TestAgentReadAny covers the DEX_EMBED_ENGINE=none fallback: FTS OR-matching
// recalls a terse finding that AND-matching (AgentRead) misses.
func TestAgentReadAny(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	if _, err := s.AgentPost(ctx, "peer-1", "", "finding", "assemble overflows the tool-result cap"); err != nil {
		t.Fatalf("post: %v", err)
	}

	// A natural-language query shares only one word ("overflow") — AND matches none.
	andHits, err := s.AgentRead(ctx, "", "finding", "why did the pack overflow downstream", 0, 10)
	if err != nil {
		t.Fatalf("AgentRead: %v", err)
	}
	if len(andHits) != 0 {
		t.Fatalf("AND matcher unexpectedly recalled %d (should miss)", len(andHits))
	}

	orHits, err := s.AgentReadAny(ctx, "", "finding", "why did the pack overflow downstream", 0, 10)
	if err != nil {
		t.Fatalf("AgentReadAny: %v", err)
	}
	if len(orHits) != 1 {
		t.Fatalf("OR matcher recalled %d, want 1", len(orHits))
	}
}

// TestAgentVecDeleteCascade asserts the AFTER DELETE trigger drops the vector
// when its message is removed, so recall can't surface a ghost.
func TestAgentVecDeleteCascade(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	if _, err := s.AgentPostVec(ctx, "peer-1", "", "finding", "ephemeral", []float32{1, 0, 0, 0}); err != nil {
		t.Fatalf("post: %v", err)
	}
	// Prune everything (cutoff in the future) → trigger must cascade to the vecs.
	if _, err := s.AgentPrune(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("prune: %v", err)
	}
	got, err := s.AgentQueryVec(ctx, []float32{1, 0, 0, 0}, "finding", 5, 0.2)
	if err != nil {
		t.Fatalf("query after clear: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("stale vector survived delete: %d hits", len(got))
	}
}
