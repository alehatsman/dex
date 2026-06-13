package retrieve

import (
	"context"
	"errors"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alehatsman/dex/internal/rerank"
	"github.com/alehatsman/dex/internal/store"
)

// poolOf builds a candidate pool of n hits with distinguishable paths and
// content. RRFScore is pre-set so tests can assert it survives the rerank.
func poolOf(n int) []store.Hit {
	hits := make([]store.Hit, n)
	for i := range hits {
		hits[i] = store.Hit{
			Path:     "f" + strconv.Itoa(i) + ".go",
			Kind:     "fn",
			Content:  "rerank candidate " + strconv.Itoa(i),
			RRFScore: float32(n - i),
		}
	}
	return hits
}

// reverseReranker returns the input docs in reversed order with descending
// scores so a test can verify RerankFused actually reorders the pool.
type reverseReranker struct{}

func (reverseReranker) Rerank(_ context.Context, _ string, docs []string) ([]rerank.Score, error) {
	out := make([]rerank.Score, len(docs))
	for i := range docs {
		out[i] = rerank.Score{
			Index: len(docs) - 1 - i,
			Score: 1.0 - float32(i)/float32(len(docs)),
		}
	}
	return out, nil
}

// unreachableReranker mirrors what a network-down rerank.Client returns.
type unreachableReranker struct{}

func (unreachableReranker) Rerank(_ context.Context, _ string, _ []string) ([]rerank.Score, error) {
	return nil, rerank.ErrUnreachable
}

// countingReranker counts Rerank calls so a test can prove the cache
// short-circuits the second identical call.
type countingReranker struct{ calls atomic.Int64 }

func (c *countingReranker) Rerank(_ context.Context, _ string, docs []string) ([]rerank.Score, error) {
	c.calls.Add(1)
	out := make([]rerank.Score, len(docs))
	for i := range docs {
		out[i] = rerank.Score{Index: i, Score: 1.0 - float32(i)/float32(len(docs))}
	}
	return out, nil
}

// hangingReranker blocks until ctx is cancelled, then returns the ctx error.
type hangingReranker struct{}

func (hangingReranker) Rerank(ctx context.Context, _ string, _ []string) ([]rerank.Score, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestRerankFusedReorders(t *testing.T) {
	svc := Service{Rerank: reverseReranker{}, RerankCache: NewRerankCache(0)}
	pool := poolOf(10)
	const k = 5

	out, err := svc.RerankFused(context.Background(), "rerank candidate", pool, k)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != k {
		t.Fatalf("got %d hits, want %d", len(out), k)
	}
	// reverseReranker ranks the last pool entry first.
	if out[0].Path != "f9.go" {
		t.Errorf("top hit = %q, want f9.go (reverse stub did not reorder)", out[0].Path)
	}
	for i, h := range out {
		if h.RerankScore <= 0 {
			t.Errorf("out[%d] %q: RerankScore = %v, want > 0", i, h.Path, h.RerankScore)
		}
		if i > 0 && out[i-1].RerankScore < h.RerankScore {
			t.Errorf("not sorted desc by RerankScore at i=%d: %v < %v", i, out[i-1].RerankScore, h.RerankScore)
		}
		if h.RRFScore <= 0 {
			t.Errorf("out[%d] %q: RRFScore = %v, want > 0 (must survive rerank)", i, h.Path, h.RRFScore)
		}
	}
}

func TestRerankFusedUnreachableFallsBack(t *testing.T) {
	svc := Service{Rerank: unreachableReranker{}, RerankCache: NewRerankCache(0)}
	pool := poolOf(10)
	const k = 5

	out, err := svc.RerankFused(context.Background(), "rerank candidate", pool, k)
	if err != nil {
		t.Fatalf("unreachable reranker must not surface as error: %v", err)
	}
	if len(out) != k {
		t.Fatalf("got %d hits, want %d", len(out), k)
	}
	// Fallback preserves the pre-rerank order and sets no RerankScore.
	for i, h := range out {
		if h.Path != pool[i].Path {
			t.Errorf("out[%d] = %q, want %q (should equal pre-rerank order)", i, h.Path, pool[i].Path)
		}
		if h.RerankScore != 0 {
			t.Errorf("out[%d] %q: RerankScore = %v, want 0 (rerank didn't run)", i, h.Path, h.RerankScore)
		}
	}
}

func TestRerankFusedCacheHits(t *testing.T) {
	rr := &countingReranker{}
	svc := Service{Rerank: rr, RerankCache: NewRerankCache(0)}
	pool := poolOf(10)
	const k = 5

	if _, err := svc.RerankFused(context.Background(), "candidate", pool, k); err != nil {
		t.Fatal(err)
	}
	if got := rr.calls.Load(); got != 1 {
		t.Fatalf("first call: invocations = %d, want 1", got)
	}
	// Identical (query, pool) → cache hit, no second network call.
	if _, err := svc.RerankFused(context.Background(), "candidate", pool, k); err != nil {
		t.Fatal(err)
	}
	if got := rr.calls.Load(); got != 1 {
		t.Errorf("second call: invocations = %d, want 1 (cache hit)", got)
	}
	// Different query → cache miss.
	if _, err := svc.RerankFused(context.Background(), "different", pool, k); err != nil {
		t.Fatal(err)
	}
	if got := rr.calls.Load(); got != 2 {
		t.Errorf("different query: invocations = %d, want 2", got)
	}
}

func TestRerankFusedTimeoutDegrades(t *testing.T) {
	svc := Service{Rerank: hangingReranker{}, RerankTimeout: 50 * time.Millisecond, RerankCache: NewRerankCache(0)}
	pool := poolOf(10)

	start := time.Now()
	out, err := svc.RerankFused(context.Background(), "candidate", pool, 5)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("RerankFused returned err = %v; expected graceful fallback", err)
	}
	if len(out) == 0 {
		t.Error("returned 0 hits; should have degraded to fused order")
	}
	if elapsed > time.Second {
		t.Errorf("took %s with RerankTimeout=50ms; timeout did not bound the call", elapsed)
	}
	for _, h := range out {
		if h.RerankScore != 0 {
			t.Errorf("hit %q has RerankScore=%v after timeout; expected fallback path", h.Path, h.RerankScore)
		}
	}
}

func TestRerankTimeoutDoesNotMaskCallerCancel(t *testing.T) {
	// If the outer ctx is cancelled, the error must not be reinterpreted as
	// ErrUnreachable — it's the caller's intent, not a backend outage.
	svc := Service{Rerank: hangingReranker{}, RerankTimeout: 5 * time.Second, RerankCache: NewRerankCache(0)}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	_, err := svc.RerankFused(ctx, "candidate", poolOf(10), 5)
	if err == nil {
		t.Fatal("expected an error when outer ctx is cancelled")
	}
	if errors.Is(err, rerank.ErrUnreachable) {
		t.Errorf("outer cancel was rewritten to ErrUnreachable: %v", err)
	}
}

func TestRerankCacheLRUEviction(t *testing.T) {
	c := NewRerankCache(2)
	c.Put("a", []rerank.Score{{Index: 0, Score: 0.5}})
	c.Put("b", []rerank.Score{{Index: 0, Score: 0.5}})
	c.Put("c", []rerank.Score{{Index: 0, Score: 0.5}})

	if _, ok := c.Get("a"); ok {
		t.Error("a should have been evicted (cap=2, inserted a→b→c)")
	}
	if _, ok := c.Get("b"); !ok {
		t.Error("b should still be present")
	}
	if _, ok := c.Get("c"); !ok {
		t.Error("c should still be present")
	}
	// Touch b so a fourth insert evicts c (now LRU).
	_, _ = c.Get("b")
	c.Put("d", []rerank.Score{{Index: 0, Score: 0.5}})
	if _, ok := c.Get("c"); ok {
		t.Error("c should have been evicted after touching b then inserting d")
	}
	if _, ok := c.Get("b"); !ok {
		t.Error("b should still be present after eviction of c")
	}
}

func TestRerankDocsCacheKey(t *testing.T) {
	docs := []string{"alpha", "beta", "gamma"}
	base := rerankDocsCacheKey("q", docs)

	if base != rerankDocsCacheKey("q", []string{"alpha", "beta", "gamma"}) {
		t.Error("identical (query, docs) produced different keys")
	}
	if base == rerankDocsCacheKey("other", docs) {
		t.Error("different queries hashed to the same key")
	}
	// Doc order is significant — Score.Index maps positionally onto docs, so a
	// reordered pool is NOT interchangeable and must key differently.
	if base == rerankDocsCacheKey("q", []string{"gamma", "beta", "alpha"}) {
		t.Error("reordered docs hashed to the same key; order must be significant")
	}
	// Length-prefixing must prevent adjacent-doc confusion.
	if rerankDocsCacheKey("q", []string{"ab", "c"}) == rerankDocsCacheKey("q", []string{"a", "bc"}) {
		t.Error("adjacent docs were confusable across the boundary")
	}
}
