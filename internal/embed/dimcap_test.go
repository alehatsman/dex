package embed

import (
	"context"
	"math"
	"strings"
	"testing"
)

// stubEmbedder returns fixed-width vectors for testing.
type stubEmbedder struct {
	model string
	vecs  [][]float32
}

func (s *stubEmbedder) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	out := make([][]float32, len(inputs))
	for i := range inputs {
		v := make([]float32, len(s.vecs[i%len(s.vecs)]))
		copy(v, s.vecs[i%len(s.vecs)])
		out[i] = v
	}
	return out, nil
}
func (s *stubEmbedder) Health(_ context.Context) error { return nil }
func (s *stubEmbedder) Endpoint() string               { return "stub" }
func (s *stubEmbedder) ModelName() string              { return s.model }
func (s *stubEmbedder) BatchSize() int                 { return 32 }
func (s *stubEmbedder) EmbedConcurrency() int          { return 1 }

func TestWithDimCap_noopWhenZero(t *testing.T) {
	inner := &stubEmbedder{model: "m", vecs: [][]float32{{1, 2, 3, 4}}}
	got := WithDimCap(inner, 0)
	if got != inner {
		t.Fatal("dim=0 should return the original embedder unchanged")
	}
}

func TestWithDimCap_truncatesAndNormalises(t *testing.T) {
	inner := &stubEmbedder{model: "m", vecs: [][]float32{{3, 4, 5, 6}}}
	wrapped := WithDimCap(inner, 2)

	if wrapped.ModelName() != "m@2" {
		t.Fatalf("ModelName = %q, want %q", wrapped.ModelName(), "m@2")
	}

	vecs, err := wrapped.Embed(context.Background(), []string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs[0]) != 2 {
		t.Fatalf("dim = %d, want 2", len(vecs[0]))
	}
	// After truncation to {3,4} the L2 norm is 5; normalised = {0.6, 0.8}.
	want := []float32{3.0 / 5, 4.0 / 5}
	for i, v := range vecs[0] {
		if math.Abs(float64(v-want[i])) > 1e-5 {
			t.Errorf("vecs[0][%d] = %v, want %v", i, v, want[i])
		}
	}
}

// TestWithDimCap_shortVectorErrors guards #458: when the model emits fewer dims
// than the configured cap, the cap can't be satisfied. Silently normalising the
// short vector and tagging it "model@<cap>" hides a model-dim change from
// EnsureEmbedModel, so Embed must fail loud instead.
func TestWithDimCap_shortVectorErrors(t *testing.T) {
	inner := &stubEmbedder{model: "m", vecs: [][]float32{{1, 0}}} // emits 2 dims
	wrapped := WithDimCap(inner, 1024)                            // cap > emitted dim

	_, err := wrapped.Embed(context.Background(), []string{"x"})
	if err == nil {
		t.Fatal("expected an error when model output is shorter than the dim cap")
	}
	if !strings.Contains(err.Error(), "1024") || !strings.Contains(err.Error(), "2") {
		t.Errorf("error should name cap (1024) and emitted dim (2): %v", err)
	}
}
