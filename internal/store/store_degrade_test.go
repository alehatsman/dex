package store

import (
	"testing"
	"time"
)

// TestDegradeNilVectorBM25Only locks the store-level degradation primitive:
// when the embedding service is offline the caller passes a nil query vector
// + the query text, and search must run BM25-only through the fusion path
// rather than erroring. This is the floor the MCP serve/search tools degrade
// onto (store.go scoreSemantic: "empty query vector means no semantic leg").
//
// If a refactor turns the nil-vector path back into a hard error, this fails.
func TestDegradeNilVectorBM25Only(t *testing.T) {
	st, ctx := newStore(t)
	now := time.Now()
	rows := []PendingChunk{
		{Path: "auth.go", Kind: "fn", Name: "Authenticate", StartLine: 1, EndLine: 2,
			ContentSHA: "h1", Content: "func Authenticate(token string) error { return validateToken(token) }",
			Vec: []float32{1, 0, 0, 0}},
		{Path: "math.go", Kind: "fn", Name: "Add", StartLine: 1, EndLine: 2,
			ContentSHA: "h2", Content: "func Add(a, b int) int { return a + b }",
			Vec: []float32{0, 1, 0, 0}},
		{Path: "log.go", Kind: "fn", Name: "Log", StartLine: 1, EndLine: 2,
			ContentSHA: "h3", Content: "func Log(msg string) { print(msg) }",
			Vec: []float32{0, 0, 1, 0}},
	}
	if err := st.UpsertMany(ctx, rows, now); err != nil {
		t.Fatal(err)
	}

	// nil vector + query text → BM25-only. Must NOT error and must still
	// return the lexically-matching chunk.
	hits, err := st.Search(ctx, nil, "Authenticate token", 5)
	if err != nil {
		t.Fatalf("degraded search (nil vec) returned error: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("degraded search (nil vec) returned no hits; BM25 floor is broken")
	}
	if hits[0].Path != "auth.go" {
		t.Errorf("top BM25 hit = %q, want auth.go", hits[0].Path)
	}

	// SearchFused (the path the mcp search tools fuse extra legs onto) must
	// also tolerate a nil vector.
	if _, err := st.SearchFused(ctx, nil, "Authenticate token", 5); err != nil {
		t.Fatalf("SearchFused (nil vec) returned error: %v", err)
	}

	// The primitive underneath: an empty query vector is "no semantic leg",
	// not an error.
	sem, err := st.scoreSemantic(ctx, nil, 5)
	if err != nil {
		t.Fatalf("scoreSemantic(nil) returned error: %v", err)
	}
	if len(sem) != 0 {
		t.Errorf("scoreSemantic(nil) returned %d scores, want 0 (no semantic leg)", len(sem))
	}
}
