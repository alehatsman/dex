package store

import (
	"testing"
	"time"
)

// TestSeenTimeMonotonicAcrossBackwardClock is the regression test for
// dex #32. It reproduces a backward wall-clock step between two index
// runs and asserts the deleted file's chunk is still pruned.
//
// Without the monotonic SeenTime guard, run2's cutoff (the earlier wall
// time) is numerically smaller than the rows run1 stamped, so the strict
// `last_seen_at < cutoff` prune leaves the stale chunk behind.
func TestSeenTimeMonotonicAcrossBackwardClock(t *testing.T) {
	st, ctx := newStore(t)

	// Run 1 stamps at a "late" wall time.
	late := time.Unix(100, 0)
	t1, err := st.SeenTime(ctx, late)
	if err != nil {
		t.Fatal(err)
	}
	rows := []PendingChunk{
		{Path: "keep.go", Kind: "fn", StartLine: 1, EndLine: 2, ContentSHA: "h1", Content: "func Keep(){}", Vec: []float32{1, 0, 0, 0}},
		{Path: "gone.go", Kind: "fn", StartLine: 1, EndLine: 2, ContentSHA: "h2", Content: "func Gone(){}", Vec: []float32{0, 1, 0, 0}},
	}
	if err := st.UpsertMany(ctx, rows, t1); err != nil {
		t.Fatal(err)
	}

	// Run 2: gone.go deleted, wall clock stepped BACKWARD (earlier than run 1).
	early := time.Unix(50, 0)
	t2, err := st.SeenTime(ctx, early)
	if err != nil {
		t.Fatal(err)
	}
	if !t2.After(t1) {
		t.Fatalf("SeenTime not monotonic: t2=%d must exceed t1=%d despite earlier wall clock",
			t2.UnixNano(), t1.UnixNano())
	}
	// Re-stamp only the surviving file.
	if err := st.UpsertMany(ctx, rows[:1], t2); err != nil {
		t.Fatal(err)
	}
	pruned, err := st.PruneUnseen(ctx, t2)
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 1 {
		t.Errorf("expected 1 pruned (gone.go), got %d", pruned)
	}
	stats, _ := st.Stats(ctx)
	if stats.Chunks != 1 {
		t.Errorf("expected 1 surviving chunk (keep.go), got %d", stats.Chunks)
	}
}

// TestGraphSeenTimeMonotonicAcrossBackwardClock is the graph-side
// regression for dex #32 — same scenario, graph_nodes/edges.
func TestGraphSeenTimeMonotonicAcrossBackwardClock(t *testing.T) {
	st, ctx := newStore(t)

	late := time.Unix(100, 0)
	t1, err := st.GraphSeenTime(ctx, late)
	if err != nil {
		t.Fatal(err)
	}
	nodes := []GraphNodeRow{
		{ID: "type:Keep", Kind: "type", Name: "Keep", QualifiedName: "Keep", PackagePath: "p", FilePath: "keep.go", ContentHash: "h1"},
		{ID: "type:Gone", Kind: "type", Name: "Gone", QualifiedName: "Gone", PackagePath: "p", FilePath: "gone.go", ContentHash: "h2"},
	}
	if err := st.GraphUpsertNodes(ctx, nodes, t1); err != nil {
		t.Fatal(err)
	}

	early := time.Unix(50, 0) // wall clock stepped backward
	t2, err := st.GraphSeenTime(ctx, early)
	if err != nil {
		t.Fatal(err)
	}
	if !t2.After(t1) {
		t.Fatalf("GraphSeenTime not monotonic: t2=%d must exceed t1=%d", t2.UnixNano(), t1.UnixNano())
	}
	if err := st.GraphUpsertNodes(ctx, nodes[:1], t2); err != nil { // re-stamp survivor only
		t.Fatal(err)
	}
	prunedNodes, _, err := st.GraphPruneUnseen(ctx, t2)
	if err != nil {
		t.Fatal(err)
	}
	if prunedNodes != 1 {
		t.Errorf("expected 1 node pruned (Gone), got %d", prunedNodes)
	}
	n, _, _ := st.GraphStats(ctx)
	if n != 1 {
		t.Errorf("expected 1 surviving node (Keep), got %d", n)
	}
}

// TestSeenTimeForwardClockUnaffected confirms normal forward time is
// passed through unchanged (no needless +1 drift when the clock behaves).
func TestSeenTimeForwardClockUnaffected(t *testing.T) {
	st, ctx := newStore(t)
	t1, err := st.SeenTime(ctx, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertMany(ctx, []PendingChunk{
		{Path: "a.go", Kind: "fn", StartLine: 1, EndLine: 2, ContentSHA: "h", Content: "x", Vec: []float32{1, 0, 0, 0}},
	}, t1); err != nil {
		t.Fatal(err)
	}
	// A later wall time exceeds the stored max, so it's used verbatim.
	want := time.Unix(200, 0)
	t2, err := st.SeenTime(ctx, want)
	if err != nil {
		t.Fatal(err)
	}
	if t2.UnixNano() != want.UnixNano() {
		t.Errorf("forward clock should pass through: got %d, want %d", t2.UnixNano(), want.UnixNano())
	}
}
