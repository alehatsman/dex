//go:build !onnx

package embed

import "errors"

// ErrONNXNotBuilt is returned by NewONNX in the default build. The ONNX engine
// is opt-in behind -tags onnx to keep the default binary dependency-free (no
// onnxruntime / tokenizer modules compiled in). Rebuild with:
//
//	GO_TAGS="sqlite_fts5 onnx" mooncake task install
var ErrONNXNotBuilt = errors.New("onnx engine not built in: rebuild with -tags onnx (e.g. GO_TAGS=\"sqlite_fts5 onnx\")")

// NewONNX is the stub implementation compiled into the default (no-onnx)
// build. It always fails, pointing the operator at the build tag. The real
// engine lives in onnx_engine.go behind //go:build onnx.
func NewONNX(_ ONNXConfig) (Embedder, error) {
	return nil, ErrONNXNotBuilt
}
