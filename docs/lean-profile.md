# Lean profile — dex with no GPU

dex normally wants an embedding backend (ollama / an OpenAI-compatible server)
and, for `ask`/`file_view` summaries, a chat model. The **lean profile** is the
opposite end of the capability ladder ([Rung C](vision.md#rung-c--useful-with-no-gpu)):
run dex with **zero inference stack** — no GPU, no embedding server — and keep
the lanes that need no inference at all: **BM25 (full-text), exact-symbol
lookup, and the pre-computed call/import graph**.

This is the "drop-in tool you `go install` and it Just Works" deployment:
laptops, CI, containers, air-gapped boxes.

## The two lean forms

| Form | Build | Embedder | Semantic search | Tool surface |
|------|-------|----------|-----------------|--------------|
| **CPU-ONNX** | `-tags onnx` | in-process ONNX on CPU | yes, on CPU | full |
| **BM25-only** | default | none (`DEX_EMBED_ENGINE=none`) | no | lexical + symbol + graph + degraded `ask` |

Both keep the **default build byte-identical** — the ONNX engine is gated behind
a build tag exactly like `sqlite_fts5` (see the build-tag matrix in
[CLAUDE.md](../CLAUDE.md)), and `DEX_EMBED_ENGINE=none` is a one-line runtime
branch. Neither adds a dependency to the default build graph.

### Form 1 — CPU-ONNX (semantic search with no GPU)

The in-process ONNX embedder (issue #180) restores the semantic lane on CPU.
Build with the tag and point dex at an operator-provided model + the
onnxruntime shared library:

```sh
GO_TAGS="sqlite_fts5 onnx" mooncake task install

export DEX_EMBED_ENGINE=onnx
export DEX_ONNXRUNTIME_LIB=/path/to/libonnxruntime.so   # dlopen'd at startup
export DEX_ONNX_MODEL=/path/to/model.onnx
export DEX_ONNX_TOKENIZER=/path/to/tokenizer.json
export DEX_ONNX_DIM=384
export DEX_ONNX_MODEL_ID=bge-small-en-v1.5

dex index .
dex mcp        # full tool surface, semantic lane served from CPU
```

A small embedder (e.g. bge-small, 384-dim) plus Matryoshka dim truncation
(#249) and int8 quantization (#215) keep CPU inference and sqlite-vec storage
cheap. ONNX vectors live in a distinct namespace (`onnx:<id>:<dim>`), so they
never mix with ollama/HTTP vectors — the store's `EnsureEmbedModel` guard forces
a reindex if you switch engines. See [CLAUDE.md](../CLAUDE.md) for the full ONNX
runtime config and the inference-correctness golden test.

### Form 2 — BM25-only (no embedder at all)

When you have no embedder and don't want one, declare it:

```sh
export DEX_EMBED_ENGINE=none
dex mcp
```

This is the **serve/query** mode. dex serves an existing index over MCP with no
embedder wired. What works with zero inference:

- `search_grep` — BM25 / FTS5 full-text search
- `search_symbol` — exact identifier lookup
- `graph_neighbors`, `graph_callers`, `graph_callees`, `graph_path`,
  `graph_deps`, `graph_cycles`, `graph_communities`, `graph_smells` — the
  pre-computed call/import graph (built at index time, no query-time inference)
- `file_tree`, `file_view` (signatures view; summaries need a chat model),
  `ctx_shell`, `ctx_prefetch` (graph spreading-activation), and the `ctx_*`
  knowledge/session tools
- `ask` — the router still works; it **degrades** to the symbol + graph lanes
  and returns a hint to use `search_grep`/`search_symbol` for the rest

## Capability-derived tool exposure (#283)

The lean profile is the proof case for **deriving the MCP tool surface from what
is actually reachable** rather than a hand-maintained tier split. With no
embedder wired, the embedding-backed tools are **not advertised at all**:

- `search_semantic`, `search_similar`, `ctx_overview`, `search_context`,
  `search_workspace`, `spec_check`

An agent connected to a lean server simply never sees a semantic tool it can't
use — no failed calls, no wasted tool-description tokens. The same mechanism
already gates `file_view` summaries on a reachable **chat** model
(`chatAvailable`); the lean profile adds the symmetric `embedAvailable` gate.

This is an **explicit declaration** (`DEX_EMBED_ENGINE=none`), not a startup
reachability probe. A probe under GPU/network contention measures load, not
capability, and would make the surface flaky — the same determinism discipline
the [perf bench](perf-bench.md) and [measurement layer](vision.md) follow.

`dex mcp` reports the mode in `status`: `model: "none (lean profile)"`,
`reachable: false`.

## Honest limits

- **No semantic ranking in BM25-only form.** Conceptual / intent queries
  ("where do we handle retries?") are weaker than full semantic search. Use
  Form 1 (CPU-ONNX) if you need the semantic lane with no GPU.
- **`file_view` summaries and `ask` synthesis need a chat model.** Without one
  they degrade to the signatures view and raw evidence respectively.

## Retrieval quality (#305)

Measured on the dex project — 304 golden queries, k=10, using the
`dex bench eval` harness (#247). The BM25-only lane is deterministic and runs
in CI with zero inference. ONNX and full-stack numbers require runtime
artifacts and are tracked separately.

| Lane | NDCG@10 | Recall@10 | MRR | Inference |
|------|---------|-----------|-----|-----------|
| **BM25-only** (`DEX_EMBED_ENGINE=none`) | **0.539** | **0.582** | **0.685** | none |
| Full stack (semantic+BM25 fused, GPU) | 0.572 | 0.622 | 0.718 | GPU embedder |
| CPU-ONNX | — | — | — | opt-in, see below |

BM25-only captures ~94% of full-stack NDCG at zero inference cost. The gap is
primarily on conceptual / intent queries where embedding similarity helps.
Exact-symbol and call-graph queries are unaffected.

**CI gate** — the BM25-only lane is gated in regression checks:

```sh
dex bench eval . --lane bm25 --check benchmark/eval/bm25-baseline.json
```

The baseline is stored at `benchmark/eval/bm25-baseline.json`. A 2% tolerance
is applied; failures indicate either index corruption or a retrieval regression.

**ONNX lane** — run where `DEX_ONNX_MODEL` and `DEX_ONNXRUNTIME_LIB` are set:

```sh
dex bench eval . --lane onnx --check benchmark/eval/onnx-baseline.json
```

Skips silently when the env vars are absent. Record your numbers in
`benchmark/eval/onnx-baseline.json` after a first run.
