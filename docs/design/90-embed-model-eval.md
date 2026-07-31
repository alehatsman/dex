# Findings — #90 Re-evaluate the default embed model

Status: **measured / decided (this round)** · Follows the #77 RFC Tier-0 hypothesis.

## Hypothesis (from #77)

The #77 benchmark showed `qwen3-embedding:0.6b` is slow on ollama and a bge-family
model runs several times faster on the same server. Tier-0 asked: **if a faster
model holds retrieval quality, switch the default** — the biggest measured
throughput lever, no new dependency. Eval-gated: throughput is the constraint,
recall is the product.

## What was measured

Clean A/B on the dex repo, same (stale) golden set of 335 queries so both rows
are directly comparable. ~14187 embeds/repo (6738 chunks + 7449 graph nodes),
`dex reindex` wall-clock on MPS. Harness: `benchmark/eval/sweep-embed-model.sh`;
raw rows in `benchmark/eval/embed-ab.tsv`.

| Model | index_s | NDCG@10 | Recall@10 | MRR |
|---|---|---|---|---|
| **qwen3-embedding:0.6b** (default) | 535 | **0.521** | **0.538** | **0.714** |
| **bge-large** | 343 (**1.56× faster**) | **0.142** | 0.232 | 0.160 |

## Verdict: do NOT swap the default to bge-large

bge-large indexes 1.56× faster but retrieval **collapses ~73%** (NDCG 0.521 →
0.142). The throughput win is real and worthless — the fast model is fast partly
because it is a small, general-English-text model weak on code.

## Why — two root causes, both structural

1. **Domain fit.** `qwen3-embedding` is a code/multilingual-strong embedder;
   `bge-large-en-v1.5` is general English prose. On a code golden set the gap is
   large regardless of speed.
2. **dex has no per-model query-instruction layer.** dex embeds documents with a
   structural stamp (`chunk_embed.go` — `path/kind/name` header) and queries with
   expansion only (`internal/retrieve/expand.go:169`). It applies **no
   model-specific retrieval instruction.** Asymmetric-retrieval models —
   bge-large-en (`"Represent this sentence for searching relevant passages:"`),
   nomic (`search_query:` / `search_document:`) — *require* that prefix or their
   query/document spaces don't align, which is exactly the collapse seen here.
   So bge-large's number is partly a usage mismatch, not purely model quality —
   but that mismatch is inherent to a **drop-in** swap.

## Consequence for #77 / reindex speed

The model-swap lever for reindex throughput is **closed** as a drop-in:
- The one fast candidate fails the quality gate hard.
- Same-size-as-qwen3 alternatives (bge-m3 ≈ 568M, the family of dex's own
  reranker) offer little throughput headroom — the win came from *small* models,
  which are the weak ones.
- Unlocking faster/asymmetric models first requires a **per-model prompt-template
  / instruction layer** in dex (new small subsystem) — a prerequisite, worth its
  own issue only if throughput pressure justifies it. Not built here.

**Redirect the reindex-speed effort** to the levers that survive: #77 Tier-1
(engage the dead `DEX_EMBED_CONCURRENCY` path + per-backend batch defaults) and
the graph-node second pass (#91). Keep `qwen3-embedding` as the default.

## Reproduce

```
git worktree add /tmp/dex-embed-ab main    # isolate the index
ollama pull bge-large                       # + any other candidate
benchmark/eval/sweep-embed-model.sh /tmp/dex-embed-ab "qwen3-embedding:0.6b bge-large"
```

## Follow-ups (not done here)

- **Per-model query-instruction layer** — prerequisite for fairly evaluating
  asymmetric models (bge-*, nomic). Own issue if pursued.
- **`dex bench embed`** — fold `sweep-embed-model.sh` into a first-class bench
  subcommand alongside `dex bench eval/nav/perf` if this becomes routine.
