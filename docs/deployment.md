# Deployment

dex needs at most three model backends, all optional and all
OpenAI-compatible (ollama, vLLM, llama.cpp, or a hosted endpoint). Pick the
profile that matches your hardware.

## Backends

| Role | Required? | Env | Used by |
|------|-----------|-----|---------|
| Embeddings | for semantic search | `DEX_EMBED_URL` (default `http://localhost:11434`), `DEX_EMBED_MODEL` | indexing, `find`, `ask` |
| Chat | optional | `DEX_CHAT_URL`, `DEX_CHAT_MODEL` | `ask` answer synthesis, `read --mode=summary` |
| Reranker | optional | `DEX_RERANK_URL`, `DEX_RERANK_MODEL` | reorders the top candidate pool |

Each degrades gracefully when its endpoint is unreachable: no chat → evidence
bundle without prose; no reranker → fused order; no embedder → BM25 + symbol +
graph. `dex doctor` probes all of them and reports what's wired.

## Profiles

**Full (GPU).** A running embedder is the only hard requirement. With ollama on
`localhost:11434`, `dex setup` auto-detects a usable embedding model. Add a chat
model for `ask` answers and a reranker for best ranking.

**Lean (no GPU).** `DEX_EMBED_ENGINE=none` drops embeddings entirely: retrieval
runs on BM25 + exact symbol + call-graph lanes with **zero inference** — ideal
for laptops, CI, containers, and air-gapped boxes. `find` is hidden; `ask`
routes across the remaining lanes. Nothing to pull, nothing to serve.

**In-process embeddings (CPU, no server).** Build with `-tags onnx` and set
`DEX_EMBED_ENGINE=onnx` to embed locally via onnxruntime — semantic search with
no embedding server. Requires operator-provided `DEX_ONNXRUNTIME_LIB`,
`DEX_ONNX_MODEL`, `DEX_ONNX_TOKENIZER`, `DEX_ONNX_DIM`; nothing is bundled or
auto-downloaded. ONNX vectors live in a distinct index namespace.

## Model selection

Any OpenAI-compatible model works; these are sane defaults:

- **Embeddings** — a code-aware embedding model (e.g. `bge`-class, or a
  `qwen3-embedding` variant). The index pins the dimension, so changing models
  means `dex reindex`.
- **Chat** (`ask` / `read summary`) — a non-thinking instruct/coder model.
  Thinking models can leave the response body empty; prefer a plain coder model.
- **Reranker** — a cross-encoder reranker if you have the VRAM; it is pure upside
  on ranking quality and optional.

Build tags: the default binary needs `sqlite_fts5` (FTS5/BM25, already required);
`onnx` is additive for the in-process embedder. Build via `mooncake task install`
— it threads the tags automatically.
