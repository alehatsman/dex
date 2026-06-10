// ONNX engine configuration and constructor seam. This file is compiled in
// EVERY build (no build tag) so callers (cmd/dex wiring) can reference
// ONNXConfig and call NewONNX regardless of whether the engine is linked in.
//
// The actual implementation lives in:
//   - onnx_engine.go      //go:build onnx   — real onnxruntime-backed engine
//   - onnx_stub.go        //go:build !onnx  — returns a "not built in" error
//
// Gating mirrors the project's sqlite_fts5 pattern: the default binary is
// byte-identical (the onnxruntime + tokenizer deps are only *compiled* under
// -tags onnx), and `-tags onnx` pulls the in-process embedder. See CLAUDE.md
// "Build-tag matrix".

package embed

// ONNXConfig is the operator-provided configuration for the in-process ONNX
// embedder. Everything is supplied explicitly (no bundling, no auto-download)
// — see issue #180 design notes. Paths typically come from env vars wired in
// cmd/dex (DEX_ONNX_MODEL, DEX_ONNX_TOKENIZER, DEX_ONNXRUNTIME_LIB).
type ONNXConfig struct {
	// ModelPath is the path to the .onnx model graph (required).
	ModelPath string
	// TokenizerPath is the path to the HuggingFace tokenizer.json (required).
	TokenizerPath string
	// LibPath is the path to the onnxruntime shared library
	// (libonnxruntime.so / .dylib / onnxruntime.dll). The yalue binding
	// dlopen's it at runtime. If empty, the binding's platform default lookup
	// is used.
	LibPath string

	// ModelID is the human-readable model identity baked into ModelName()
	// (e.g. "bge-small-en-v1.5"). Combined with Dim it forms the namespace
	// string "onnx:<ModelID>:<Dim>" recorded in the index meta table, so an
	// ONNX-built index can never be silently mixed with ollama/http vectors
	// (the store's EnsureEmbedModel guard trips on mismatch).
	ModelID string
	// Dim is the embedding dimension the model outputs (e.g. 384). Used both
	// for the namespace string and to size output tensors.
	Dim int
	// MaxSeqLen caps tokens per input; longer inputs are truncated. 0 => 512.
	MaxSeqLen int
	// Batch is the per-call chunk size the indexer loops over. 0 => 32.
	Batch int

	// Input/output tensor names. Defaults target the common BERT-style
	// sentence-transformers export (input_ids/attention_mask/token_type_ids
	// -> last_hidden_state, mean-pooled). Override per model via env.
	InputIDsName    string // default "input_ids"
	AttentionName   string // default "attention_mask"
	TokenTypeName   string // default "token_type_ids"
	OutputName      string // default "last_hidden_state"
	NeedsTokenTypes bool   // some models (e.g. nomic) omit token_type_ids input
}
