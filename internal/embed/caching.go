package embed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// cacheFormatVersion is folded into every cache key. Bump it when the cached
// vector encoding or the key recipe changes, so entries written by an older
// dex miss cleanly instead of being served wrong.
const cacheFormatVersion = "v1"

// VecStore is the minimal content-addressed vector cache CachingEmbedder
// depends on; internal/veccache implements it over sqlite. Declared here so
// the embed package stays free of a storage dependency.
type VecStore interface {
	// Get returns the cached vector for each key that is present; missing
	// keys are absent from the map. An error tells the caller to treat every
	// input as a miss (embed all).
	Get(ctx context.Context, keys []string) (map[string][]float32, error)
	// Put stores vectors for the given keys. Best-effort: an error is
	// non-fatal to indexing.
	Put(ctx context.Context, entries map[string][]float32) error
}

// WithCache wraps em so repeated embeds of identical text — the common case
// across reindex runs — are served from store instead of recomputed (#121).
// It caches the FINAL vectors em returns, so it must wrap the OUTERMOST
// embedder in the chain (outside dim-cap / normalisation). A nil store or a
// nil em returns em unchanged, so callers degrade to no-cache on open failure.
func WithCache(em Embedder, store VecStore) Embedder {
	if store == nil || em == nil {
		return em
	}
	return &cachingEmbedder{inner: em, store: store, modelTag: em.ModelName()}
}

// cachingEmbedder is a read-through/write-through cache decorator over an
// Embedder. The cache key binds the model identity (so a model or dim-cap
// swap misses) to the literal input text (so any change in how text is
// composed — chunking, the context prefix, node signatures — misses too).
type cachingEmbedder struct {
	inner    Embedder
	store    VecStore
	modelTag string
}

func (c *cachingEmbedder) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	if len(inputs) == 0 {
		return c.inner.Embed(ctx, inputs)
	}
	keys := make([]string, len(inputs))
	for i, t := range inputs {
		keys[i] = c.key(t)
	}
	// Best-effort read: a store error means treat everything as a miss.
	hits, err := c.store.Get(ctx, keys)
	if err != nil {
		hits = nil
	}
	out := make([][]float32, len(inputs))
	var missIdx []int
	var missText []string
	for i := range inputs {
		if v, ok := hits[keys[i]]; ok {
			out[i] = v
			continue
		}
		missIdx = append(missIdx, i)
		missText = append(missText, inputs[i])
	}
	if len(missText) == 0 {
		return out, nil
	}
	vecs, err := c.inner.Embed(ctx, missText)
	if err != nil {
		return nil, err
	}
	if len(vecs) != len(missText) {
		return nil, fmt.Errorf("embed cache: backend returned %d vectors for %d inputs", len(vecs), len(missText))
	}
	put := make(map[string][]float32, len(missText))
	for j, gi := range missIdx {
		out[gi] = vecs[j]
		put[keys[gi]] = vecs[j]
	}
	// Best-effort write; a failure never fails the embed.
	_ = c.store.Put(ctx, put)
	return out, nil
}

// key = sha256(modelTag ⧺ 0 ⧺ formatVersion ⧺ 0 ⧺ text), hex-encoded.
func (c *cachingEmbedder) key(text string) string {
	h := sha256.New()
	h.Write([]byte(c.modelTag))
	h.Write([]byte{0})
	h.Write([]byte(cacheFormatVersion))
	h.Write([]byte{0})
	h.Write([]byte(text))
	return hex.EncodeToString(h.Sum(nil))
}

func (c *cachingEmbedder) Health(ctx context.Context) error { return c.inner.Health(ctx) }
func (c *cachingEmbedder) Endpoint() string                 { return c.inner.Endpoint() }
func (c *cachingEmbedder) ModelName() string                { return c.inner.ModelName() }
func (c *cachingEmbedder) BatchSize() int                   { return c.inner.BatchSize() }
func (c *cachingEmbedder) EmbedConcurrency() int            { return c.inner.EmbedConcurrency() }
