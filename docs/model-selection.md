# Model Selection Guide

Practical advice for choosing embedding, chat, and reranker models for
dex. Benchmarks are from MTEB (mid-2026 leaderboard), hardware figures from
RTX 5090 / 5080 inference benchmarks. All VRAM figures assume the default Ollama
quantization (Q4_K_M GGUF) unless noted.

## Role map

| Role | Env var | What it does |
|---|---|---|
| Embed | `DEX_EMBED_MODEL` | All search queries + indexing |
| Chat/Ask | `DEX_CHAT_MODEL` | Foreground Q&A via `ask` |
| Rerank | `DEX_RERANK_MODEL` | Post-retrieval scoring of top-40 candidates |
| Compress | `DEX_COMPRESS_MODEL` | Context compression via `ctx_shell` / `dex compress` |
| Draft | `DEX_DRAFT_MODEL` | Speculative drafts for `generate` |

---

## Embedding models

### MTEB Code NDCG@10 comparison

| Model | MTEB Code | VRAM (Q4) | Dim | Context |
|---|---|---|---|---|
| **Qwen3-Embedding-0.6B** | 75.41 | ~1.5 GB | 1024 | 32K |
| **Qwen3-Embedding-4B** *(default)* | 80.06 | ~5 GB | 2560 | 32K |
| Qwen3-Embedding-8B | 80.68 | ~8 GB | 4096 | 32K |
| BGE-M3 (dense) | 58.22 | ~1 GB | 1024 | 8K |
| NV-Embed-v2 | 63.74 | ~24 GB peak | 4096 | 32K |
| mxbai-embed-large | ~64.5 | ~0.7 GB | 1024 | 512 |
| nomic-embed-text | ~63.8 | ~0.5 GB | 768 | 8K |

**Recommendation:** Stay on **Qwen3-Embedding-4B** (default). The gap over all
pre-Qwen3 models is large and real — even the 0.6B variant beats NV-Embed-v2
by 11+ NDCG points at 1/16 the VRAM.

Drop to **0.6B** only if VRAM is tight alongside other foreground models; the
5-point quality loss is acceptable. The 8B gives +0.6 points over 4B — not worth
3× the VRAM.

**Do not use** BGE-M3 hybrid mode for code. Dense Qwen3 already beats BGE-M3
hybrid by 12–17 points. If you want a hybrid boost, BM25 (already built in) is
free and adds 3–5% for zero cost.

### Quantization

Store vectors at **FP16** on GPU — INT8 vector storage is 4× slower on GPU
than FP16 due to kernel overhead. Q4_K_M model weights lose 4–6% retrieval
quality vs FP16; Q8_0 is near-lossless at 50% VRAM.

### Dim change = full reindex

The `chunk_vecs` schema is declared with a fixed dimension at index creation
time. Switching embed model families (e.g. 4B→8B changes 2560→4096 dims)
requires `dex reindex`.

---

## Chat / Ask models

### Benchmark snapshot (foreground chat, code tasks)

| Model | VRAM (Q5_K_M) | t/s RTX 5090 | Ctx headroom | EvalPlus |
|---|---|---|---|---|
| **Qwen3-Coder-30B-A3B** *(recommended)* | ~22 GB | ~220 t/s | 64K+ | 86.6% |
| Qwen2.5-Coder-32B Q4_K_M | ~27.8 GB | 48–62 t/s | 33K effective | ~66% |
| Qwen3-32B Q4_K_M | ~18.6 GB | ~61 t/s | 64K | ~82% |
| Qwen3-14B Q5_K_M | ~12.9 GB | ~59 t/s (5080) | 32K | — |
| Qwen3-8B Q4_K_M | ~4.8 GB | ~185 t/s | 32K | — |

**Foreground ask (RTX 5090):** Use **`Qwen3-Coder-30B-A3B`** (MoE, 3B active).
Same VRAM slot as the current 32B, 4× faster TTFT, 2× usable context, 20+
points better on code benchmarks.

```
Qwen/Qwen3-Coder-30B-A3B-Instruct          # BF16 via vLLM
Qwen/Qwen3-Coder-30B-A3B-Instruct-AWQ      # AWQ via vLLM (fastest batch)
unsloth/Qwen3-Coder-Next-GGUF              # Q5_K_M GGUF for Ollama
```

**Non-thinking mode is mandatory for ask/chat** — append `/no_think` or pass
`enable_thinking=False`. Thinking mode generates 2–5× more tokens with no
benefit for code-search Q&A.

### Compress (RTX 5090)

**Qwen3-8B Q4_K_M** (~4.8 GB, ~185 t/s on 5090). Trivially fits alongside
30B-A3B under time-multiplexing. Instruction-following quality is more than
sufficient for compression; non-thinking mode.

### Recommended `.dex/config.yml`

```yaml
endpoints:
  chat:    http://127.0.0.1:8081   # 5090 — Qwen3-Coder-30B-A3B (vLLM)
  compress: http://127.0.0.1:8081  # 5090

models:
  embed:   qwen3-embedding:4b
  chat:    Qwen/Qwen3-Coder-30B-A3B-Instruct
  compress: Qwen/Qwen3-8B
```

---

## Rerank models

**The current default (`BAAI/bge-reranker-v2-m3`) is wrong for code.** It
scores 41.38 MTEB-Code — it was trained on multilingual text pairs, not code
corpora. This is the highest-ROI upgrade available.

### MTEB-Code comparison

| Model | MTEB-Code | VRAM (Q4) | Latency 40 docs (5090) |
|---|---|---|---|
| **BGE-reranker-v2-m3** *(current default)* | 41.38 | ~2 GB | 50–100ms |
| Qwen3-Reranker-0.6B | 73–75 | ~0.5 GB Q4 | 200–500ms |
| **Qwen3-Reranker-4B** *(recommended)* | **81.20** | ~3 GB Q4 | 1–2s |
| Qwen3-Reranker-8B | 81.22 | ~5 GB Q4 | 2–4s |

**Recommendation: Qwen3-Reranker-4B.** Doubles MTEB-Code quality vs the current
default, fits the 5s rerank timeout, fits on both GPUs.

```
Qwen/Qwen3-Reranker-4B                   # BF16 via vLLM
Mungert/Qwen3-Reranker-4B-GGUF           # Q8_0 (~4.5 GB) or Q4_K_M (~2.5 GB)
```

Serve via vLLM with `task="score"` and empty `<think>\n\n</think>` prefill
(non-thinking mode). Set `DEX_RERANK_STYLE=chat-vllm` to enable the prefill
path already implemented in dex.

```yaml
endpoints:
  rerank: http://127.0.0.1:8083   # vLLM serving Qwen3-Reranker-4B
models:
  rerank: Qwen/Qwen3-Reranker-4B
tools:
  rerank_style: chat-vllm
```

**Fallback (latency-sensitive or VRAM-constrained):** Qwen3-Reranker-0.6B Q4_K_M
(~500 MB, sub-500ms) still beats the current default by 32 MTEB-Code points.

### Why not ColBERT?

ColBERT late-interaction is not a drop-in for the reranker slot — it replaces
both the ANN retrieval and reranking stages, requiring index architecture
changes, a PLAID index build, and a custom serving path. Not worth the
operational complexity given Qwen3-Reranker-4B already achieves 81.20
MTEB-Code with the existing infrastructure.

### Why not SPLADE?

SPLADE is a first-stage retriever (learned sparse vectors), not a reranker.
It can improve recall@40 before reranking (complementary to BM25), but it
cannot substitute the cross-encoder/LLM-reranker step.

---

## Quantization cheat sheet

| Quant | VRAM vs FP16 | Quality retention | When to use |
|---|---|---|---|
| FP16 | 1× | 100% | Default when VRAM allows |
| Q8_0 | 0.5× | 98–99% | Near-lossless; tight VRAM |
| **Q5_K_M** | 0.39× | ~99% | **Sweet spot for 14B on 5080** |
| Q4_K_M | 0.25× | 92–96% | VRAM-constrained; acceptable for compress |
| Q4_0 / INT4 | 0.25× | 90–94% | Avoid for embed/rerank; last resort for generation |

---

## GPU allocation summary (RTX 5090)

| GPU | Model | Role | VRAM |
|---|---|---|---|
| RTX 5090 (32 GB) | Qwen3-Embedding-4B | embed | ~5 GB |
| RTX 5090 | Qwen3-Coder-30B-A3B Q5_K_M | ask / chat | ~22 GB |
| RTX 5090 | Qwen3-8B Q4_K_M | compress | ~5 GB |
| RTX 5090 | Qwen3-Reranker-4B Q4 | rerank | ~3 GB |

5090 peak load (embed + ask + compress time-multiplexed): ~27 GB — fits with
~5 GB headroom for KV cache. Reranker shares the ask slot (rerank runs before
the ask call returns, not concurrently with it).
