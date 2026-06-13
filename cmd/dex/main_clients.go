package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/alehatsman/dex/internal/chat"
	"github.com/alehatsman/dex/internal/embed"
	"github.com/alehatsman/dex/internal/rerank"
)

func newEmbedClient(indexModel string) embed.Embedder {
	// Engine selection is explicit and static per index (no hot swap):
	// ONNX vectors live in a different space than ollama/http vectors, so an
	// index must be built and queried with the same engine. DEX_EMBED_ENGINE
	// defaults to "http" (the OpenAI-compatible backend). "onnx" selects the
	// in-process engine, which is only linked in -tags onnx builds (otherwise
	// embed.NewONNX returns a clear "rebuild with -tags onnx" error).
	if indexModel == "bm25-only" {
		return nil
	}
	switch strings.ToLower(os.Getenv("DEX_EMBED_ENGINE")) {
	case "onnx":
		return newONNXEmbedder()
	case "none":
		// Lean / zero-infra profile (#290): no embedder is wired at all.
		// Returns a nil Embedder so the MCP server omits the embedding-backed
		// tools (search_semantic/search_similar/search_context/ctx_overview/
		// search_workspace) and ask degrades to the symbol + graph + BM25
		// lanes. Explicit declaration, not a startup probe — deterministic
		// under GPU contention. See docs/lean-profile.md.
		fmt.Fprintln(os.Stderr, "dex: DEX_EMBED_ENGINE=none — lean profile, no embedder (BM25 + symbol + graph only)")
		return nil
	}

	url := os.Getenv("DEX_EMBED_URL")
	model := os.Getenv("DEX_EMBED_MODEL")

	if url == "" {
		ensureOllamaRunning() // best-effort: start ollama if installed-but-down
		if om, ok := embed.DetectOllama(context.Background()); ok {
			url = om.URL
			if model == "" {
				model = om.Name
			}
			fmt.Fprintf(os.Stderr, "dex: ollama embed model %q at %s\n", model, url)
		} else {
			url = "http://127.0.0.1:8082"
		}
	}
	// Prefer the index-recorded model over the probe/hard-coded default when
	// DEX_EMBED_MODEL is not set — prevents silent dim mismatches on multi-model
	// setups (e.g. nomic 768d index queried with qwen3 2560d).
	if model == "" && indexModel != "" {
		model = indexModel
		fmt.Fprintf(os.Stderr, "dex: using index-recorded embed model %q\n", model)
	}
	if model == "" {
		model = "Qwen/Qwen3-Embedding-4B"
	}

	batch := 32
	if explicit := os.Getenv("DEX_EMBED_BATCH"); explicit != "" {
		if v, err := strconv.Atoi(explicit); err != nil || v <= 0 {
			fmt.Fprintf(os.Stderr, "warning: DEX_EMBED_BATCH=%q is not a positive integer; using 32\n", explicit)
		} else {
			batch = v
		}
	} else {
		// No explicit batch size — probe VRAM and pick a suitable default.
		if vram := embed.FreeVRAMGB(); vram > 0 {
			batch = embed.BatchSizeForVRAM(vram, 32)
		}
	}
	conc := envInt("DEX_EMBED_CONCURRENCY", 4)
	timeout := parseDuration("DEX_EMBED_TIMEOUT", envOr("DEX_EMBED_TIMEOUT", "60s"), 60*time.Second)
	c := embed.NewWithConcurrency(url, model, batch, conc, timeout)
	return embed.WithDimCap(c, envInt("DEX_EMBED_DIM", 0))
}

// newONNXEmbedder builds the in-process ONNX embedder from operator-provided
// env vars. The engine is opt-in behind -tags onnx; in a default build
// embed.NewONNX returns ErrONNXNotBuilt and we exit with a clear message
// rather than silently degrading (the operator explicitly asked for onnx).
func newONNXEmbedder() embed.Embedder {
	cfg := embed.ONNXConfig{
		ModelPath:       os.Getenv("DEX_ONNX_MODEL"),
		TokenizerPath:   os.Getenv("DEX_ONNX_TOKENIZER"),
		LibPath:         os.Getenv("DEX_ONNXRUNTIME_LIB"),
		ModelID:         envOr("DEX_ONNX_MODEL_ID", "model"),
		Dim:             envInt("DEX_ONNX_DIM", 0),
		MaxSeqLen:       envInt("DEX_ONNX_MAX_SEQ", 512),
		Batch:           envInt("DEX_EMBED_BATCH", 32),
		InputIDsName:    os.Getenv("DEX_ONNX_INPUT_IDS"),
		AttentionName:   os.Getenv("DEX_ONNX_ATTENTION"),
		TokenTypeName:   os.Getenv("DEX_ONNX_TOKEN_TYPE"),
		OutputName:      os.Getenv("DEX_ONNX_OUTPUT"),
		NeedsTokenTypes: envBool("DEX_ONNX_TOKEN_TYPES", true),
	}
	em, err := embed.NewONNX(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dex: onnx embed engine: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "dex: onnx embed engine %q (dim %d) from %s\n", em.ModelName(), cfg.Dim, cfg.ModelPath)
	return em
}

func newChatClient() *chat.Client {
	url := os.Getenv("DEX_CHAT_URL")
	model := os.Getenv("DEX_CHAT_MODEL")

	if url == "" {
		ensureOllamaRunning() // best-effort: start ollama if installed-but-down
		if om, ok := embed.DetectOllamaChat(context.Background()); ok {
			url = om.URL
			if model == "" {
				model = om.Name
			}
			fmt.Fprintf(os.Stderr, "dex: ollama chat model %q at %s\n", model, url)
		} else {
			url = "http://127.0.0.1:8081"
		}
	}
	if model == "" {
		model = "Qwen/Qwen2.5-Coder-7B-Instruct"
	}
	timeout := parseDuration("DEX_CHAT_TIMEOUT", envOr("DEX_CHAT_TIMEOUT", "120s"), 120*time.Second)
	return chat.New(url, model, timeout)
}

// newRerankClient returns a rerank.HealthChecker (either the Cohere-compatible
// *rerank.Client or the decoder-style *rerank.ChatReranker), or nil when
// reranking is disabled. Rerank is OFF by default — empty DEX_RERANK_URL
// or DEX_DISABLE_RERANK=1 yields nil; store.Search treats nil as
// "skip the stage".
//
// DEX_RERANK_STYLE selects the backend:
//
//	"chat-vllm" (default) — vLLM + Qwen3-Reranker with <think> assistant prefill
//	"chat"               — chat-completions + logprobs (ollama / standard chat servers)
//	"cohere"             — Cohere-compatible /rerank endpoint (TEI, Infinity, vLLM
//	                       with a cross-encoder model like bge-reranker-v2-m3)
func newRerankClient() rerank.HealthChecker {
	url := os.Getenv("DEX_RERANK_URL")
	if url == "" {
		return nil
	}
	if os.Getenv("DEX_DISABLE_RERANK") == "1" {
		return nil
	}
	model := envOr("DEX_RERANK_MODEL", "Qwen/Qwen3-Reranker-4B")
	timeout := parseDuration("DEX_RERANK_TIMEOUT", envOr("DEX_RERANK_TIMEOUT", "5s"), 5*time.Second)
	style := envOr("DEX_RERANK_STYLE", "chat-vllm")
	if style == "chat" || style == "chat-vllm" {
		rawConc := envOr("DEX_RERANK_CONCURRENCY", "16")
		concurrency, cerr := strconv.Atoi(rawConc)
		if cerr != nil || concurrency <= 0 {
			fmt.Fprintf(os.Stderr, "warning: DEX_RERANK_CONCURRENCY=%q is not a positive integer; using 16\n", rawConc)
			concurrency = 16
		}
		c := rerank.NewChat(url, model, concurrency, timeout)
		// chat-vllm: enable <think> assistant prefill for Qwen3-Reranker on vLLM.
		// Plain chat (ollama / standard servers) must NOT set this — they continue
		// the XML pattern and generate "<" instead of "yes"/"no".
		c.ThinkingPrefill = style == "chat-vllm"
		return c
	}
	return rerank.New(url, model, timeout)
}

// newExpandClient builds the query-side expansion client (#252). Expansion is
// opt-in: with DEX_EXPAND_MODEL unset it returns nil and the feature is a
// no-op. The endpoint defaults to the resolved chat backend (base) so on the
// standard local-ollama deployment, setting just DEX_EXPAND_MODEL=qwen3:4b is
// enough to enable it; DEX_EXPAND_URL overrides the endpoint.
func newExpandClient(base *chat.Client) *chat.Client {
	model := os.Getenv("DEX_EXPAND_MODEL")
	if model == "" {
		return nil
	}
	url := os.Getenv("DEX_EXPAND_URL")
	if url == "" && base != nil {
		url = base.BaseURL
	}
	if url == "" {
		return nil
	}
	timeout := parseDuration("DEX_EXPAND_TIMEOUT", envOr("DEX_EXPAND_TIMEOUT", "5s"), 5*time.Second)
	return chat.New(url, model, timeout)
}

// expandDefaultMode resolves the server-side default expand level. With an
// expansion client configured but DEX_EXPAND_MODE unset, expansion defaults
// to "on" (cheap lexical expansion, no extra embed); otherwise it honors the
// env value, and "off" when no client is wired.
func expandDefaultMode(client *chat.Client) string {
	if client == nil {
		return "off"
	}
	return envOr("DEX_EXPAND_MODE", "on")
}

// envInt reads a positive integer env var with a default.
// Non-positive or unparsable values fall back to def with a warning.
func envInt(name string, def int) int {
	raw := os.Getenv(name)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		fmt.Fprintf(os.Stderr, "warning: %s=%q is not a non-negative integer; using %d\n", name, raw, def)
		return def
	}
	return n
}

// rerankPool reads the candidate-pool cap from the environment.
// Default 40, clamped to [1, 100]. Larger = better recall but slower
// cross-encoder call. Only consulted when a Reranker is wired.
func rerankPool() int {
	raw := envOr("DEX_RERANK_POOL", "40")
	pool, err := strconv.Atoi(raw)
	if err != nil || pool <= 0 {
		fmt.Fprintf(os.Stderr, "warning: DEX_RERANK_POOL=%q is not a positive integer; using 40\n", raw)
		pool = 40
	}
	if pool > 100 {
		pool = 100
	}
	return pool
}

// ─── index ─────────────────────────────────────────────────────────────────

// acquireProjectLock takes the per-project indexer lock. cmdName labels
// the holder ("index"/"reindex"/"watch") and phase reports
// the current pipeline stage. wait blocks until the lock is free;
// breakLock discards an existing lockfile (only safe when the prior
// holder is gone — a live flock cannot be stolen).
//
// On contention without --wait or --break-lock, prints a friendly
// "another dex is busy here" line and returns (nil, nil) so the caller
// can exit 0. On any other failure, returns the error.
