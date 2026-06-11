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
		if len(v) > d.dim {
			v = v[:d.dim]
		}
		l2Normalize(v)
		vecs[i] = v
	}
	return vecs, nil
}

func (d *dimCapEmbedder) Health(ctx context.Context) error { return d.inner.Health(ctx) }
func (d *dimCapEmbedder) Endpoint() string                 { return d.inner.Endpoint() }
func (d *dimCapEmbedder) BatchSize() int                   { return d.inner.BatchSize() }

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
