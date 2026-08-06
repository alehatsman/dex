package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// seedGraphLaneFixture populates a store with a.go (semantically on the query,
// session-touched) and b.go (orthogonal to the query, reachable only via the
// graph edge a.go→b.go). For a query along (1,0,0,0) the spreading-activation
// lane is the only thing that pulls b.go into the hit set.
func seedGraphLaneFixture(t *testing.T, ctx context.Context, st *Store) {
	t.Helper()
	now := time.Now()
	if err := st.UpsertMany(ctx, []PendingChunk{
		{Path: "a.go", Kind: "fn", StartLine: 1, EndLine: 5, ContentSHA: "ha",
			Content: "func A(){}", Vec: []float32{1, 0, 0, 0}},
		{Path: "b.go", Kind: "fn", StartLine: 1, EndLine: 5, ContentSHA: "hb",
			Content: "func B(){}", Vec: []float32{0, 1, 0, 0}},
	}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.GraphUpsertNodes(ctx, []GraphNodeRow{
		{ID: "n:a", Kind: "file", Name: "a.go", FilePath: "a.go", ContentHash: "ha"},
		{ID: "n:b", Kind: "file", Name: "b.go", FilePath: "b.go", ContentHash: "hb"},
	}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.GraphUpsertEdges(ctx, []GraphEdgeRow{
		{ID: "e:ab", Kind: "calls", SrcID: "n:a", DstID: "n:b",
			FilePath: "a.go", ContentHash: "eab"},
	}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.SessionAddFile(ctx, "a.go", "read"); err != nil {
		t.Fatal(err)
	}
}

func graphFixtureStore(t *testing.T, ctx context.Context, name string, opts Options) *Store {
	t.Helper()
	st, err := OpenWith(ctx, filepath.Join(t.TempDir(), name), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	seedGraphLaneFixture(t, ctx, st)
	return st
}

func hasPath(hits []Hit, path string) bool {
	for _, h := range hits {
		if h.Path == path {
			return true
		}
	}
	return false
}

// TestFuseSpreadingActivationDisabled verifies the GraphLaneDisabled switch
// (#470 ablation) holds the graph-proximity lane out of fusion entirely:
// FuseSpreadingActivation returns the primary hits unchanged even when graph +
// session data would otherwise expand the set. This is the true "lane off" the
// weight cannot express — GraphLaneWeight = 0 is the "unset → default 1.0"
// sentinel — so the graph-sweep eval needs the flag to measure a real graph-off
// baseline. The control store (lane on) proves the fixture actually expands, so
// the disabled assertion is a real regression guard, not a vacuous one.
func TestFuseSpreadingActivationDisabled(t *testing.T) {
	ctx := context.Background()
	primary := []Hit{{Path: "a.go", StartLine: 1, EndLine: 5, Score: 1.0}}
	queryVec := []float32{1, 0, 0, 0}

	// Control: lane on (default options) — spreading activation adds b.go.
	on := graphFixtureStore(t, ctx, "on.db", Options{})
	expanded := on.FuseSpreadingActivation(ctx, primary, queryVec, 5)
	if !hasPath(expanded, "b.go") {
		t.Fatalf("control (lane on): expected graph lane to add b.go, got %d hits", len(expanded))
	}

	// Subject: lane disabled — FuseSpreadingActivation is a no-op.
	off := graphFixtureStore(t, ctx, "off.db",
		Options{GraphOptions: GraphOptions{GraphLaneDisabled: true}})
	got := off.FuseSpreadingActivation(ctx, primary, queryVec, 5)
	if len(got) != len(primary) {
		t.Fatalf("lane disabled: got %d hits, want %d (no expansion)", len(got), len(primary))
	}
	if got[0].Path != "a.go" {
		t.Errorf("lane disabled: hit[0]=%q, want a.go (primary unchanged)", got[0].Path)
	}
	if hasPath(got, "b.go") {
		t.Error("lane disabled: b.go present — graph lane was not held out")
	}
}
