package embed

import (
	"context"
	"fmt"
	"math"
)

type dimCapEmbedder struct {
	inner Embedder
	dim   int
}

// WithDimCap wraps e so each output vector is truncated to dim elements and
// then L2-normalised. dim <= 0 returns e unchanged.
//
// Suited for Matryoshka Representation Learning (MRL) embedders (e.g.
// Qwen3-Embedding) where the first k dimensions form a valid sub-embedding:
// truncating and re-normalising preserves cosine similarity ordering.
//
// ModelName returns "model@dim" so the store's EnsureEmbedModel guard catches
// a mismatch between index-time and query-time dim caps.
func WithDimCap(e Embedder, dim int) Embedder {
	if dim <= 0 {
		return e
	}
	return &dimCapEmbedder{inner: e, dim: dim}
}

func (d *dimCapEmbedder) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	vecs, err := d.inner.Embed(ctx, inputs)
	if err != nil {
		return nil, err
	}
	for i, v := range vecs {
		switch {
		case len(v) > d.dim:
			// MRL: the first d.dim elements form a valid sub-embedding.
			v = v[:d.dim]
		case len(v) < d.dim:
			// The cap can't be satisfied — the model emits fewer dims than the
			// configured cap. Silently normalising the short vector would still
			// store it under the "model@<cap>" tag, leaving EnsureEmbedModel
			// blind to a later model-dim change (it compares the cap, not the
			// emitted dim). Fail loud so the misconfiguration / model swap
			// surfaces at index/query time instead of forming a dim-mismatched
			// index.
			return nil, fmt.Errorf("embed: dim cap %d exceeds model output %d (reindex or correct the cap)", d.dim, len(v))
		}
		l2Normalize(v)
		vecs[i] = v
	}
	return vecs, nil
}

func (d *dimCapEmbedder) Health(ctx context.Context) error { return d.inner.Health(ctx) }
func (d *dimCapEmbedder) Endpoint() string                 { return d.inner.Endpoint() }
func (d *dimCapEmbedder) BatchSize() int                   { return d.inner.BatchSize() }
func (d *dimCapEmbedder) EmbedConcurrency() int            { return d.inner.EmbedConcurrency() }

// ModelName encodes the dim cap so the store's EnsureEmbedModel guard
// catches a dimension mismatch between index-time and query-time configs.
func (d *dimCapEmbedder) ModelName() string {
	return fmt.Sprintf("%s@%d", d.inner.ModelName(), d.dim)
}

func l2Normalize(v []float32) {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return
	}
	inv := float32(1.0 / math.Sqrt(sum))
	for i := range v {
		v[i] *= inv
	}
}
