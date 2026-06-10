//go:build onnx

package embed

import (
	"context"
	"math"
	"os"
	"strconv"
	"testing"
)

// onnxTestConfigFromEnv builds an ONNXConfig from the same env vars the dex
// binary uses, or skips the test if the operator hasn't pointed at real
// artifacts. ONNX inference can't run without the onnxruntime shared library
// and a model on disk, so this test is a no-op in CI's compile-only onnx lane
// and only does real work on a box provisioned with both.
func onnxTestConfigFromEnv(t *testing.T) ONNXConfig {
	t.Helper()
	model := os.Getenv("DEX_ONNX_MODEL")
	tok := os.Getenv("DEX_ONNX_TOKENIZER")
	lib := os.Getenv("DEX_ONNXRUNTIME_LIB")
	if model == "" || tok == "" || lib == "" {
		t.Skip("onnx golden test needs DEX_ONNX_MODEL + DEX_ONNX_TOKENIZER + DEX_ONNXRUNTIME_LIB pointing at real artifacts")
	}
	dim := envIntTest(t, "DEX_ONNX_DIM", 0)
	if dim <= 0 {
		t.Fatal("set DEX_ONNX_DIM to the model's embedding dimension")
	}
	return ONNXConfig{
		ModelPath:       model,
		TokenizerPath:   tok,
		LibPath:         lib,
		ModelID:         envOrTest("DEX_ONNX_MODEL_ID", "test-model"),
		Dim:             dim,
		MaxSeqLen:       512,
		Batch:           8,
		NeedsTokenTypes: os.Getenv("DEX_ONNX_TOKEN_TYPES") != "false",
	}
}

// TestONNXEmbedShapeAndNorm verifies the engine produces one unit-norm vector
// of the configured dimension per input, ordering preserved across a batch.
// This is the structural correctness gate; it runs only when real artifacts
// are present (see onnxTestConfigFromEnv).
func TestONNXEmbedShapeAndNorm(t *testing.T) {
	cfg := onnxTestConfigFromEnv(t)
	em, err := NewONNX(cfg)
	if err != nil {
		t.Fatalf("NewONNX: %v", err)
	}
	if got := em.ModelName(); got == "" {
		t.Fatal("empty ModelName")
	}

	inputs := []string{
		"func main() { fmt.Println(\"hello\") }",
		"the quick brown fox",
		"a third distinct sentence about databases",
	}
	vecs, err := em.Embed(context.Background(), inputs)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vecs) != len(inputs) {
		t.Fatalf("got %d vectors, want %d", len(vecs), len(inputs))
	}
	for i, v := range vecs {
		if len(v) != cfg.Dim {
			t.Errorf("vec %d: dim %d, want %d", i, len(v), cfg.Dim)
		}
		var sumSq float64
		for _, x := range v {
			sumSq += float64(x) * float64(x)
		}
		norm := math.Sqrt(sumSq)
		if math.Abs(norm-1.0) > 1e-3 {
			t.Errorf("vec %d: L2 norm %.6f, want ~1.0 (output not normalized)", i, norm)
		}
	}

	// Determinism: same input -> same vector.
	again, err := em.Embed(context.Background(), inputs[:1])
	if err != nil {
		t.Fatalf("Embed (repeat): %v", err)
	}
	for j := range again[0] {
		if math.Abs(float64(again[0][j]-vecs[0][j])) > 1e-5 {
			t.Fatalf("non-deterministic embedding at dim %d: %v vs %v", j, again[0][j], vecs[0][j])
		}
	}
}

// TestONNXGoldenAgainstHTTP compares ONNX vectors to the HTTP/ollama backend
// serving the SAME model, within a cosine tolerance — the real correctness
// gate that the pooling/normalization matches the reference implementation.
// Needs DEX_ONNX_GOLDEN_URL (+ DEX_ONNX_GOLDEN_MODEL) in addition to the ONNX
// artifacts, so it skips unless the operator wires up both backends.
func TestONNXGoldenAgainstHTTP(t *testing.T) {
	url := os.Getenv("DEX_ONNX_GOLDEN_URL")
	if url == "" {
		t.Skip("golden comparison needs DEX_ONNX_GOLDEN_URL (http backend serving the same model)")
	}
	cfg := onnxTestConfigFromEnv(t)
	onnx, err := NewONNX(cfg)
	if err != nil {
		t.Fatalf("NewONNX: %v", err)
	}
	httpModel := envOrTest("DEX_ONNX_GOLDEN_MODEL", cfg.ModelID)
	ref := New(url, httpModel, 8, 0)

	inputs := []string{"semantic search over source code", "binary search tree balancing"}
	ov, err := onnx.Embed(context.Background(), inputs)
	if err != nil {
		t.Fatalf("onnx embed: %v", err)
	}
	rv, err := ref.Embed(context.Background(), inputs)
	if err != nil {
		t.Fatalf("http embed: %v", err)
	}
	for i := range inputs {
		cos := cosine(ov[i], rv[i])
		if cos < 0.99 {
			t.Errorf("input %d: cosine(onnx, http)=%.4f < 0.99 — pooling/normalization diverges from reference", i, cos)
		}
	}
}

func cosine(a, b []float32) float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	var dot, na, nb float64
	for i := 0; i < n; i++ {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func envOrTest(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envIntTest(t *testing.T, key string, def int) int {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		t.Fatalf("%s=%q is not an integer: %v", key, v, err)
	}
	return n
}
