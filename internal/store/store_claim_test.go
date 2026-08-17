package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/alehatsman/dex/internal/store"
)

func claim(t *testing.T, s *store.Store, agent, file, intent string) {
	t.Helper()
	if _, err := s.AgentPost(context.Background(), agent, store.NormalizeClaimPath(file), "claim", intent); err != nil {
		t.Fatalf("claim %s/%s: %v", agent, file, err)
	}
}

// TestClaimsOverlapping covers the S1 surface query: self-filter, path-suffix
// overlap, and that a non-matching path yields nothing.
func TestClaimsOverlapping(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	claim(t, s, "alice", "internal/store/store_agent.go", "adding AgentQueryVec")
	claim(t, s, "me", "internal/mcp/look.go", "editing look") // ours — must self-filter

	// A peer claim on the same file surfaces, tolerating a ./ prefix + :line.
	hits, err := s.ClaimsOverlapping(ctx, "./internal/store/store_agent.go:42", "me", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].AgentID != "alice" {
		t.Fatalf("want 1 alice claim, got %+v", hits)
	}
	if hits[0].Intent != "adding AgentQueryVec" {
		t.Errorf("intent lost: %q", hits[0].Intent)
	}

	// Our own claim is never returned to us.
	if hits, _ := s.ClaimsOverlapping(ctx, "internal/mcp/look.go", "me", time.Time{}); len(hits) != 0 {
		t.Errorf("self-claim leaked: %+v", hits)
	}

	// A different file does not overlap.
	if hits, _ := s.ClaimsOverlapping(ctx, "internal/store/store_knowledge.go", "me", time.Time{}); len(hits) != 0 {
		t.Errorf("false overlap: %+v", hits)
	}
}

// TestClaimReleaseSupersedes: a release tombstone drops the earlier claim
// (latest-per-agent+file wins), and a re-claim after release is active again.
func TestClaimReleaseSupersedes(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	const f = "internal/x.go"

	claim(t, s, "alice", f, "editing")
	if hits, _ := s.ClaimsOverlapping(ctx, f, "me", time.Time{}); len(hits) != 1 {
		t.Fatalf("claim not active: %+v", hits)
	}

	claim(t, s, "alice", f, store.ClaimReleaseMarker) // release
	if hits, _ := s.ClaimsOverlapping(ctx, f, "me", time.Time{}); len(hits) != 0 {
		t.Fatalf("release did not retract claim: %+v", hits)
	}

	claim(t, s, "alice", f, "editing again") // re-claim
	if hits, _ := s.ClaimsOverlapping(ctx, f, "me", time.Time{}); len(hits) != 1 {
		t.Fatalf("re-claim not active: %+v", hits)
	}
}

// TestActiveClaimsTTL: a claim older than the freshness cutoff is excluded.
func TestActiveClaimsTTL(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	claim(t, s, "alice", "internal/x.go", "editing")

	// since in the future → nothing is fresh enough.
	fresh, err := s.ActiveClaims(ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh) != 0 {
		t.Fatalf("stale claim survived TTL cutoff: %+v", fresh)
	}
	// zero time → all claims.
	if all, _ := s.ActiveClaims(ctx, time.Time{}); len(all) != 1 {
		t.Fatalf("want 1 active claim, got %d", len(all))
	}
}
