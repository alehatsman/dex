//go:build !onnx

package embed

import (
	"errors"
	"strings"
	"testing"
)

// TestNewONNXStub locks the default-build contract: without -tags onnx the
// engine is not linked in and NewONNX fails with a clear, actionable error
// pointing at the build tag. This runs in the default CI gate.
func TestNewONNXStub(t *testing.T) {
	em, err := NewONNX(ONNXConfig{ModelPath: "x", TokenizerPath: "y", Dim: 384})
	if em != nil {
		t.Fatal("expected nil Embedder from the stub")
	}
	if !errors.Is(err, ErrONNXNotBuilt) {
		t.Fatalf("got %v, want ErrONNXNotBuilt", err)
	}
	if !strings.Contains(err.Error(), "onnx") {
		t.Errorf("error %q should mention the onnx build tag", err)
	}
}
