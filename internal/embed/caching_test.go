package embed

import (
	"context"
	"errors"
	"testing"
)

// mapStore is an in-memory VecStore for tests, with optional injected errors
// and call counters.
type mapStore struct {
	m      map[string][]float32
	getErr error
	putErr error
}

func newMapStore() *mapStore { return &mapStore{m: map[string][]float32{}} }

func (s *mapStore) Get(_ context.Context, keys []string) (map[string][]float32, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	out := make(map[string][]float32)
	for _, k := range keys {
		if v, ok := s.m[k]; ok {
			out[k] = v
		}
	}
	return out, nil
}

func (s *mapStore) Put(_ context.Context, entries map[string][]float32) error {
	if s.putErr != nil {
		return s.putErr
	}
	for k, v := range entries {
		s.m[k] = v
	}
	return nil
}

// countingEmbedder records how many inputs it embedded. Its vectors are a
// deterministic function of the input text so a cache hit is byte-comparable
// to a fresh embed.
type countingEmbedder struct {
	model    string
	embedded int
}

func (c *countingEmbedder) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	c.embedded += len(inputs)
	out := make([][]float32, len(inputs))
	for i, s := range inputs {
		var first float32
		if len(s) > 0 {
			first = float32(s[0])
		}
		out[i] = []float32{first, float32(len(s)), 1, 0}
	}
	return out, nil
}
func (c *countingEmbedder) Health(context.Context) error { return nil }
func (c *countingEmbedder) Endpoint() string             { return "counting" }
func (c *countingEmbedder) ModelName() string            { return c.model }
func (c *countingEmbedder) BatchSize() int               { return 32 }
func (c *countingEmbedder) EmbedConcurrency() int        { return 1 }

func equalVec(a, b []float32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestWithCache_nilStoreIsPassthrough(t *testing.T) {
	inner := &countingEmbedder{model: "m@4"}
	if got := WithCache(inner, nil); got != inner {
		t.Fatal("nil store must return the embedder unchanged")
	}
}

// TestCaching_secondPassAllHits: the win — re-embedding identical text makes
// zero new backend calls, and the cached vectors equal the fresh ones.
func TestCaching_secondPassAllHits(t *testing.T) {
	inner := &countingEmbedder{model: "m@4"}
	c := WithCache(inner, newMapStore())
	ctx := context.Background()
	inputs := []string{"alpha", "beta", "gamma"}

	first, err := c.Embed(ctx, inputs)
	if err != nil {
		t.Fatal(err)
	}
	if inner.embedded != 3 {
		t.Fatalf("cold pass embedded %d, want 3", inner.embedded)
	}

	second, err := c.Embed(ctx, inputs)
	if err != nil {
		t.Fatal(err)
	}
	if inner.embedded != 3 {
		t.Fatalf("warm pass embedded more: total %d, want 3 (all hits)", inner.embedded)
	}
	for i := range inputs {
		if !equalVec(first[i], second[i]) {
			t.Errorf("input %d: cached vector != fresh vector", i)
		}
	}
}

// TestCaching_interleavedPreservesOrder: a mix of hits and misses returns
// vectors in input order and embeds only the misses.
func TestCaching_interleavedPreservesOrder(t *testing.T) {
	inner := &countingEmbedder{model: "m@4"}
	store := newMapStore()
	c := WithCache(inner, store)
	ctx := context.Background()

	if _, err := c.Embed(ctx, []string{"b", "d"}); err != nil { // warm b, d
		t.Fatal(err)
	}
	inner.embedded = 0

	out, err := c.Embed(ctx, []string{"a", "b", "c", "d"})
	if err != nil {
		t.Fatal(err)
	}
	if inner.embedded != 2 {
		t.Fatalf("embedded %d, want 2 (only a, c missed)", inner.embedded)
	}
	fresh := &countingEmbedder{model: "m@4"}
	want, _ := fresh.Embed(ctx, []string{"a", "b", "c", "d"})
	for i := range want {
		if !equalVec(out[i], want[i]) {
			t.Errorf("out[%d] mismatched — order not preserved across hits/misses", i)
		}
	}
}

// TestCaching_modelTagIsolates: two embedders with different model tags never
// share entries, even with the same store and text — the anti-corruption guard.
func TestCaching_modelTagIsolates(t *testing.T) {
	store := newMapStore()
	a := &countingEmbedder{model: "m1@4"}
	b := &countingEmbedder{model: "m2@4"}
	ctx := context.Background()

	if _, err := WithCache(a, store).Embed(ctx, []string{"x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := WithCache(b, store).Embed(ctx, []string{"x"}); err != nil {
		t.Fatal(err)
	}
	if b.embedded != 1 {
		t.Fatalf("model-b embedded %d, want 1 (a different model tag must miss)", b.embedded)
	}
}

// TestCaching_getErrorDegrades: a store read error is non-fatal — everything
// is treated as a miss and embedded live.
func TestCaching_getErrorDegrades(t *testing.T) {
	inner := &countingEmbedder{model: "m@4"}
	store := newMapStore()
	store.getErr = errors.New("boom")
	c := WithCache(inner, store)

	out, err := c.Embed(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatalf("get error must not fail the embed: %v", err)
	}
	if len(out) != 2 || inner.embedded != 2 {
		t.Fatalf("get error should embed all: embedded=%d len(out)=%d, want 2/2", inner.embedded, len(out))
	}
}

// TestCaching_putErrorDegrades: a store write error is non-fatal to the embed.
func TestCaching_putErrorDegrades(t *testing.T) {
	inner := &countingEmbedder{model: "m@4"}
	store := newMapStore()
	store.putErr = errors.New("boom")
	c := WithCache(inner, store)

	if _, err := c.Embed(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("put error must not fail the embed: %v", err)
	}
}
