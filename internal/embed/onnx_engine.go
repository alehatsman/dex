//go:build onnx

// In-process ONNX embedder. Compiled only under -tags onnx so the default
// build stays free of the onnxruntime + tokenizer modules (see onnx.go and
// CLAUDE.md "Build-tag matrix"). Implements embed.Embedder: tokenize ->
// onnxruntime session -> mean-pool over tokens (attention-masked) -> L2
// normalize, producing sentence embeddings comparable to the HTTP/ollama path
// for the same model.

package embed

import (
	"context"
	"fmt"
	"sync"

	"github.com/sugarme/tokenizer"
	"github.com/sugarme/tokenizer/pretrained"
	ort "github.com/yalue/onnxruntime_go"
)

// ortInitOnce guards process-wide onnxruntime environment init. The C runtime
// is global; InitializeEnvironment must run exactly once before any session.
var (
	ortInitOnce sync.Once
	ortInitErr  error
)

func initORT(libPath string) error {
	ortInitOnce.Do(func() {
		if libPath != "" {
			ort.SetSharedLibraryPath(libPath)
		}
		if !ort.IsInitialized() {
			ortInitErr = ort.InitializeEnvironment()
		}
	})
	return ortInitErr
}

type onnxEmbedder struct {
	cfg     ONNXConfig
	tk      *tokenizer.Tokenizer
	maxSeq  int
	batch   int
	inputs  []string
	outName string

	// session access is serialized: a single DynamicAdvancedSession is not
	// guaranteed concurrency-safe, and the indexer already drives batches
	// sequentially through Embed.
	mu   sync.Mutex
	sess *ort.DynamicAdvancedSession
}

// NewONNX builds the in-process ONNX embedder from operator-provided paths.
func NewONNX(cfg ONNXConfig) (Embedder, error) {
	if cfg.ModelPath == "" {
		return nil, fmt.Errorf("onnx: DEX_ONNX_MODEL (model .onnx path) is required")
	}
	if cfg.TokenizerPath == "" {
		return nil, fmt.Errorf("onnx: DEX_ONNX_TOKENIZER (tokenizer.json path) is required")
	}
	if cfg.Dim <= 0 {
		return nil, fmt.Errorf("onnx: embedding dim must be > 0 (set via config)")
	}
	if err := initORT(cfg.LibPath); err != nil {
		return nil, fmt.Errorf("onnx: init runtime (set DEX_ONNXRUNTIME_LIB to libonnxruntime.so): %w", err)
	}
	tk, err := pretrained.FromFile(cfg.TokenizerPath)
	if err != nil {
		return nil, fmt.Errorf("onnx: load tokenizer %q: %w", cfg.TokenizerPath, err)
	}

	idsName := orDefault(cfg.InputIDsName, "input_ids")
	maskName := orDefault(cfg.AttentionName, "attention_mask")
	typeName := orDefault(cfg.TokenTypeName, "token_type_ids")
	inputs := []string{idsName, maskName}
	if cfg.NeedsTokenTypes {
		inputs = append(inputs, typeName)
	}
	outName := orDefault(cfg.OutputName, "last_hidden_state")

	opts, err := ort.NewSessionOptions()
	if err != nil {
		return nil, fmt.Errorf("onnx: session options: %w", err)
	}
	defer func() { _ = opts.Destroy() }()

	sess, err := ort.NewDynamicAdvancedSession(cfg.ModelPath, inputs, []string{outName}, opts)
	if err != nil {
		return nil, fmt.Errorf("onnx: load model %q: %w", cfg.ModelPath, err)
	}

	maxSeq := cfg.MaxSeqLen
	if maxSeq <= 0 {
		maxSeq = 512
	}
	batch := cfg.Batch
	if batch <= 0 {
		batch = 32
	}
	return &onnxEmbedder{
		cfg:     cfg,
		tk:      tk,
		maxSeq:  maxSeq,
		batch:   batch,
		inputs:  inputs,
		outName: outName,
		sess:    sess,
	}, nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// Embed tokenizes inputs, runs the session one batch at a time, mean-pools the
// token embeddings under the attention mask, and L2-normalizes. Output order
// matches input order.
func (e *onnxEmbedder) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	out := make([][]float32, len(inputs))
	for start := 0; start < len(inputs); start += e.batch {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := start + e.batch
		if end > len(inputs) {
			end = len(inputs)
		}
		vecs, err := e.embedBatch(inputs[start:end])
		if err != nil {
			return nil, err
		}
		copy(out[start:end], vecs)
	}
	return out, nil
}

func (e *onnxEmbedder) embedBatch(texts []string) ([][]float32, error) {
	b := len(texts)
	// Tokenize each text; truncate to maxSeq. Track the longest sequence so
	// the batch is right-padded to a rectangular [B, L] shape.
	idsList := make([][]int64, b)
	maskList := make([][]int64, b)
	maxLen := 1
	for i, t := range texts {
		enc, err := e.tk.EncodeSingle(t, true)
		if err != nil {
			return nil, fmt.Errorf("onnx: tokenize: %w", err)
		}
		ids := toInt64(enc.GetIds())
		mask := toInt64(enc.GetAttentionMask())
		if len(ids) > e.maxSeq {
			ids = ids[:e.maxSeq]
			mask = mask[:e.maxSeq]
		}
		idsList[i] = ids
		maskList[i] = mask
		if len(ids) > maxLen {
			maxLen = len(ids)
		}
	}

	flatIDs := make([]int64, b*maxLen)
	flatMask := make([]int64, b*maxLen)
	flatType := make([]int64, b*maxLen) // all zeros (single-sequence)
	for i := 0; i < b; i++ {
		copy(flatIDs[i*maxLen:], idsList[i])
		copy(flatMask[i*maxLen:], maskList[i])
	}

	shape := ort.NewShape(int64(b), int64(maxLen))
	idsTensor, err := ort.NewTensor(shape, flatIDs)
	if err != nil {
		return nil, fmt.Errorf("onnx: ids tensor: %w", err)
	}
	defer idsTensor.Destroy()
	maskTensor, err := ort.NewTensor(shape, flatMask)
	if err != nil {
		return nil, fmt.Errorf("onnx: mask tensor: %w", err)
	}
	defer maskTensor.Destroy()

	inputVals := []ort.Value{idsTensor, maskTensor}
	if e.cfg.NeedsTokenTypes {
		typeTensor, err := ort.NewTensor(shape, flatType)
		if err != nil {
			return nil, fmt.Errorf("onnx: token_type tensor: %w", err)
		}
		defer typeTensor.Destroy()
		inputVals = append(inputVals, typeTensor)
	}

	outputs := []ort.Value{nil} // nil => session allocates the output tensor
	e.mu.Lock()
	runErr := e.sess.Run(inputVals, outputs)
	e.mu.Unlock()
	if runErr != nil {
		return nil, fmt.Errorf("onnx: run: %w", runErr)
	}
	outTensor, ok := outputs[0].(*ort.Tensor[float32])
	if !ok {
		if outputs[0] != nil {
			outputs[0].Destroy()
		}
		return nil, fmt.Errorf("onnx: unexpected output type %T (want float32 tensor)", outputs[0])
	}
	defer outTensor.Destroy()

	return e.poolNormalize(outTensor, maskList, b, maxLen)
}

// poolNormalize mean-pools last_hidden_state [B, L, H] over the L axis using
// the attention mask, then L2-normalizes each [H] vector. This matches the
// pooling sentence-transformers/bge/nomic apply at export time.
func (e *onnxEmbedder) poolNormalize(out *ort.Tensor[float32], maskList [][]int64, b, maxLen int) ([][]float32, error) {
	dims := out.GetShape()
	data := out.GetData()
	// Accept [B, L, H]; some models emit a pre-pooled [B, H] sentence vector.
	var hidden int
	pooled := false
	switch len(dims) {
	case 3:
		if int(dims[0]) != b || int(dims[1]) != maxLen {
			return nil, fmt.Errorf("onnx: output shape %v != expected [%d %d H]", dims, b, maxLen)
		}
		hidden = int(dims[2])
	case 2:
		if int(dims[0]) != b {
			return nil, fmt.Errorf("onnx: pre-pooled output shape %v != expected [%d H]", dims, b)
		}
		hidden = int(dims[1])
		pooled = true
	default:
		return nil, fmt.Errorf("onnx: unsupported output rank %d (shape %v)", len(dims), dims)
	}
	if hidden != e.cfg.Dim {
		return nil, fmt.Errorf("onnx: model hidden dim %d != configured dim %d (DEX_ONNX_DIM)", hidden, e.cfg.Dim)
	}

	res := make([][]float32, b)
	for i := 0; i < b; i++ {
		vec := make([]float32, hidden)
		if pooled {
			copy(vec, data[i*hidden:(i+1)*hidden])
		} else {
			var count float32
			for j := 0; j < maxLen; j++ {
				if j >= len(maskList[i]) || maskList[i][j] == 0 {
					continue
				}
				count++
				base := (i*maxLen + j) * hidden
				for h := 0; h < hidden; h++ {
					vec[h] += data[base+h]
				}
			}
			if count > 0 {
				inv := 1.0 / count
				for h := range vec {
					vec[h] *= inv
				}
			}
		}
		l2Normalize(vec)
		res[i] = vec
	}
	return res, nil
}

func toInt64(xs []int) []int64 {
	out := make([]int64, len(xs))
	for i, x := range xs {
		out[i] = int64(x)
	}
	return out
}

// Health reports the engine is loaded and ready. The model is loaded eagerly
// in NewONNX, so reaching here means the session exists; there is no network
// dependency to probe (that is the whole point of the offline engine).
func (e *onnxEmbedder) Health(_ context.Context) error {
	if e.sess == nil {
		return fmt.Errorf("onnx: session not initialized")
	}
	return nil
}

func (e *onnxEmbedder) Endpoint() string { return "onnx:" + e.cfg.ModelPath }
func (e *onnxEmbedder) BatchSize() int   { return e.batch }

// ModelName is the index namespace identity: "onnx:<modelID>:<dim>". The
// "onnx:" prefix and explicit dim ensure an ONNX-built index is never silently
// mixed with ollama/http vectors — the store's EnsureEmbedModel guard trips on
// any mismatch and demands a reindex.
func (e *onnxEmbedder) ModelName() string {
	id := e.cfg.ModelID
	if id == "" {
		id = "model"
	}
	return fmt.Sprintf("onnx:%s:%d", id, e.cfg.Dim)
}
