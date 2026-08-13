# Deployment

dex needs at most three model backends, all optional and all
OpenAI-compatible (ollama, vLLM, llama.cpp, or a hosted endpoint). Pick the
profile that matches your hardware.

## Backends

| Role | Required? | Env | Used by |
|------|-----------|-----|---------|
| Embeddings | for semantic search | `DEX_EMBED_URL` (default `http://localhost:11434`), `DEX_EMBED_MODEL` | indexing, `ask` semantic lane |
| Chat | optional | `DEX_CHAT_URL`, `DEX_CHAT_MODEL` | `ask` answer synthesis, `read --mode=summary` |
| Reranker | optional | `DEX_RERANK_URL`, `DEX_RERANK_MODEL` | reorders the top candidate pool |

Each degrades gracefully when its endpoint is unreachable: no chat → evidence
bundle without prose; no reranker → fused order; no embedder → BM25 + symbol +
graph. `dex doctor` probes all of them for liveness and reports what's wired.
For a stronger signal, `dex doctor --deep` sends one minimal real request per
configured backend (embed a string, a 1-token completion, rerank a pair) and
reports usable / model-not-ready / unreachable / cold-timeout — it can load a
cold model, so it's opt-in and slower than the default liveness check.

## Profiles

**Full (GPU).** A running embedder is the only hard requirement. With ollama on
`localhost:11434`, `dex setup` auto-detects a usable embedding model. Add a chat
model for `ask` answers and a reranker for best ranking.

**Lean (no GPU).** `DEX_EMBED_ENGINE=none` drops embeddings entirely: retrieval
runs on BM25 + exact symbol + call-graph lanes with **zero inference** — ideal
for laptops, CI, containers, and air-gapped boxes. The semantic lane is skipped;
`ask` routes across the remaining lanes on its own. Nothing to pull, nothing to
serve.

**In-process embeddings (CPU, no server).** Build with `-tags onnx` and set
`DEX_EMBED_ENGINE=onnx` to embed locally via onnxruntime — semantic search with
no embedding server. Requires operator-provided `DEX_ONNXRUNTIME_LIB`,
`DEX_ONNX_MODEL`, `DEX_ONNX_TOKENIZER`, `DEX_ONNX_DIM`; nothing is bundled or
auto-downloaded. ONNX vectors live in a distinct index namespace.

**High-throughput GPU embeddings (infinity / CUDA).** The default HTTP engine
is OpenAI-compatible, so any `/v1/embeddings` server works — including a CUDA
[infinity](https://github.com/michaelfeil/infinity) instance for fast bulk
embedding. Point dex at it and reindex:

```
DEX_EMBED_URL=http://cuda-box:7997     # infinity's OpenAI-compatible endpoint
DEX_EMBED_MODEL=bge-small-en-v1.5      # whatever the server serves
dex reindex
```

> **Repointing `DEX_EMBED_URL` to a different serving stack — reindex required.**
> The embedding *model name* is the index identity, not the URL (URLs are
> volatile: host/port cosmetics, failover pools, and load balancers all serve
> identical vectors). So moving from, say, an ollama-GGUF backend to an
> infinity torch-fp16 backend **under the same model name** does not trip the
> `EnsureEmbedModel` guard, even though the two stacks can produce vectors in
> different spaces — mixing them silently degrades search.
>
> When you repoint at a genuinely different serving stack, either run
> `dex reindex`, or set `DEX_EMBED_MODEL` to a distinct identity tag (e.g.
> `bge-small-en-v1.5@infinity`) so the guard hard-trips and forces a rebuild.
> As a safety net, dex records the endpoint at index time and logs a one-time
> WARNING on the next index if it changed — advisory only; indexing proceeds.

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
