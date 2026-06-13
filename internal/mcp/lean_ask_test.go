package mcp

import (
	"context"
	"testing"

	"github.com/alehatsman/dex/internal/store"
)

// bm25OnlySearcher is a Searcher stub that records the query vector it receives
// and returns a single canned hit from Search. Only Search is exercised by
// runSemanticLane; the rest satisfy the interface.
type bm25OnlySearcher struct {
	gotVec    []float32
	vecWasNil bool
	called    bool
}

func (f *bm25OnlySearcher) Search(_ context.Context, queryVec []float32, _ string, _ int) ([]store.Hit, error) {
	f.called = true
	f.gotVec = queryVec
	f.vecWasNil = queryVec == nil
	return []store.Hit{{Path: "internal/watch/watch.go", StartLine: 60, EndLine: 71, Kind: "method", Name: "markDirty", RRFScore: 0.02}}, nil
}

func (f *bm25OnlySearcher) SearchFused(context.Context, []float32, string, int) ([]store.Hit, error) {
	return nil, nil
}
func (f *bm25OnlySearcher) RerankFused(context.Context, string, []store.Hit, int) ([]store.Hit, error) {
	return nil, nil
}
func (f *bm25OnlySearcher) FindSymbol(context.Context, string, int) ([]store.Hit, error) {
	return nil, nil
}
func (f *bm25OnlySearcher) FindSymbolCandidates(context.Context, string, int) ([]string, error) {
	return nil, nil
}
func (f *bm25OnlySearcher) RelatedChunks(context.Context, string, int, int) ([]store.Hit, error) {
	return nil, nil
}
func (f *bm25OnlySearcher) ChunkAt(context.Context, string, int) (store.Hit, error) {
	return store.Hit{}, nil
}

// TestLeanSemanticLaneRunsBM25 proves the lean profile (#426): with no embedder
// wired, runSemanticLane still queries the store — passing a nil vector so
// Search runs BM25-only — and returns the lexical hits rather than reporting
// the lane unreachable. The earlier behavior short-circuited to unreachable and
// left ask answerless in the headline no-GPU deployment.
func TestLeanSemanticLaneRunsBM25(t *testing.T) {
	srv := &Server{} // no EmbedClient → lean profile
	st := &bm25OnlySearcher{}

	hits, unreachable := srv.runSemanticLane(context.Background(), st, "debouncing", "debouncing", 8)

	if !st.called {
		t.Fatal("lean lane never called Search; BM25 leg was skipped")
	}
	if !st.vecWasNil {
		t.Errorf("lean lane passed a non-nil query vector %v; want nil for BM25-only", st.gotVec)
	}
	if unreachable {
		t.Error("lean lane reported unreachable; want a normal BM25 result")
	}
	if len(hits) != 1 || hits[0].Path != "internal/watch/watch.go" {
		t.Errorf("lean lane returned %v; want the single canned BM25 hit", hits)
	}
	if hits[0].Score <= 0 {
		t.Errorf("BM25-only hit surfaced Score=%v; want the RRF fallback score", hits[0].Score)
	}
}
