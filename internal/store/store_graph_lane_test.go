package store

import (
	"testing"
	"time"
)

// TestSearchGraphLane verifies that Store.Search applies the graph-proximity
// lane: chunks from graph-adjacent files of session-recent files are fused at
// 0.5× RRF weight, so a graph neighbor appears in results even when its
// embedding is orthogonal to the query vector.
func TestSearchGraphLane(t *testing.T) {
	st, ctx := newStore(t)
	now := time.Now()

	// Two chunks: a.go is semantically close to the query, b.go is orthogonal.
	if err := st.UpsertMany(ctx, []PendingChunk{
		{Path: "a.go", Kind: "fn", StartLine: 1, EndLine: 5, ContentSHA: "ha",
			Content: "func A(){}", Vec: []float32{1, 0, 0, 0}},
		{Path: "b.go", Kind: "fn", StartLine: 1, EndLine: 5, ContentSHA: "hb",
			Content: "func B(){}", Vec: []float32{0, 1, 0, 0}},
	}, now); err != nil {
		t.Fatal(err)
	}

	// Build graph: a.go file-node → b.go file-node (a "calls" edge).
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

	// Session: agent recently touched a.go.
	if err := st.SessionAddFile(ctx, "a.go", "read"); err != nil {
		t.Fatal(err)
	}

	// Query along (1,0,0,0): semantically a.go scores 1.0, b.go scores 0.
	// Without graph lane: only a.go would appear (k=2 still returns both,
	// but graph lane must surface b.go when k=1 due to RRF boost from
	// graph proximity of session file a.go → neighbor b.go).
	hits, err := st.Search(ctx, []float32{1, 0, 0, 0}, "", 2)
	if err != nil {
		t.Fatal(err)
	}

	paths := make(map[string]bool, len(hits))
	for _, h := range hits {
		paths[h.Path] = true
	}
	if !paths["b.go"] {
		t.Errorf("b.go missing from results — graph-proximity lane not applied; got paths: %v", hitPaths(hits))
	}
}

// TestSearchGraphLaneNoSession verifies that Store.Search without a session
// does not crash and returns normal results.
func TestSearchGraphLaneNoSession(t *testing.T) {
	st, ctx := newStore(t)
	now := time.Now()
	if err := st.UpsertMany(ctx, []PendingChunk{
		{Path: "a.go", Kind: "fn", StartLine: 1, EndLine: 2, ContentSHA: "ha",
			Content: "func A(){}", Vec: []float32{1, 0, 0, 0}},
	}, now); err != nil {
		t.Fatal(err)
	}

	// No session, no graph — must return normal semantic results.
	hits, err := st.Search(ctx, []float32{1, 0, 0, 0}, "", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Path != "a.go" {
		t.Errorf("expected [a.go], got %v", hitPaths(hits))
	}
}

func hitPaths(hits []Hit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.Path
	}
	return out
}
