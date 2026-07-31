# Design — #77 Batched embed backend (infinity) vs accepting the one-time reindex cost

Status: **design / investigation** · Reframes issue #77 against measured data.
Decide direction before any implementation.

## TL;DR — the measurements overturned the premise

The issue proposed standing up a batched **infinity** embed backend to fix ~40-min
first-index times, reasoning by analogy from the reranker (which runs on infinity
and batches well). I benchmarked it. **On the Mac / MPS box where the pain lives,
infinity is _slower_ than ollama for the same embedding model**, so the proposed
fix does not solve the stated problem. What the data actually says:

1. **The dominant lever is the embed model, not the backend.** The current
   default, `qwen3-embedding:0.6b`, is pathologically slow on ollama — **4–10
   chunks/s** batched, and it gets *worse* as batch grows. A bge-family model on
   the **same ollama server** does **~56 chunks/s** — a **5–13× swing from model
   choice alone**, zero infra change.

2. **infinity/torch-MPS loses to ollama/llama.cpp-Metal for the same model.**
   Clean A/B on `bge-large-en-v1.5`, MPS: ollama **56 c/s** vs infinity **33 c/s**
   (per-item 18 ms vs 31 ms). The reranker wins on infinity because there is no
   ollama cross-encoder alternative — that advantage **does not transfer** to the
   embed leg on Apple Silicon. infinity's batching win is real only on the **CUDA
   server** topology, not the 24 GB Mac.

3. **dex's `DEX_EMBED_CONCURRENCY` is genuinely inert in the chunk pass** (a real
   code finding, §3) — but engaging it is a *conditional* win: +1.88× for the
   GPU-underutilizing qwen3 case, **−23% (0.77×)** for bge-large which already
   saturates MPS single-stream. So it's worth wiring up, but it is not the
   headline fix the issue implied.

4. **The graph pass re-embeds ~35k tiny synthetic node strings** (§6) — pure
   serving overhead, and a real backend-independent dedup question sits under it.

**Recommendation:** _do not_ adopt infinity for embedding on the Mac — it's
measurably slower. Instead: (Tier 0) **evaluate switching the default embed model
away from `qwen3-embedding:0.6b`** — biggest measured win, gated on a retrieval
eval; (Tier 1) engage the dead concurrency path + a per-backend batch default;
(Tier 2) keep infinity strictly as an **opt-in, CUDA-server-only** embed lane via
`DEX_EMBED_URL`, never the default. "Accept the one-time cost + rely on the mtime
incremental path" remains the honest baseline for most repos.

---

## Evidence — measured this session

Box: 24 GB unified-memory Mac, MPS. Background load: the dex reranker
(`infinity_emb … BAAI/bge-reranker-v2-m3 --device mps` on `:8082`) resident +
apps — the realistic contended state the issue describes. ollama =
llama.cpp/Metal; infinity = PyTorch/MPS. Harness: batched POST to
`/v1/embeddings`, best-of-3; concurrency = 4 client threads × batch 128.

### ollama `qwen3-embedding:0.6b` (current default embed model)

| Shape | Throughput | Per-item |
|---|---|---|
| batch = 8 | **26.7 c/s** | 37 ms |
| batch = 32 | **4.4 c/s** | 226 ms |
| batch = 64 | 6.7 c/s | 149 ms |
| batch = 128 | 10.0 c/s | 100 ms |
| 4×128 seq (conc=1) | 10.9 c/s | — |
| 4×128 **par (conc=4)** | 20.5 c/s | **1.88×** |

qwen3-0.6b *collapses* as batch grows and is slow enough that client concurrency
recovers ~2×. This is the model dex ships as the ollama default.

### Same-model A/B — `bge-large-en-v1.5`, ollama vs infinity, MPS

| Shape | ollama (llama.cpp/Metal) | infinity (torch/MPS) |
|---|---|---|
| batch = 8 | 53.7 c/s | 34.7 c/s |
| batch = 32 | **56.0 c/s** | 35.9 c/s |
| batch = 64 | 56.2 c/s | 34.4 c/s |
| batch = 128 | 49.0 c/s | 32.7 c/s |
| batch = 256 | — | 32.6 c/s |
| per-item (flat) | **~18 ms** | ~30 ms |
| conc=4 vs seq | **0.77×** (hurts) | 0.98× (neutral) |

**ollama is ~1.7× faster than infinity for the identical model on this box.**
Both are flat across batch size (no collapse) — so qwen3's collapse above is a
*model/config* pathology, not a general ollama property. infinity's flat per-item
curve confirms true batching; it just runs on a slower MPS math path than
llama.cpp's Metal kernels, so batching can't overcome the per-op deficit here.

> **Caveat / scope of the numbers:** MPS only, one box, models differ in quality.
> `bge-small-en-v1.5` on infinity hit ~346 c/s — a *model-size* artifact (33M vs
> 335M params), not a serving win; it's excluded from the A/B for that reason. On
> the **CUDA server** (`michaelf34/infinity:latest`, GPU torch) the infinity vs
> ollama ordering may well reverse — batched torch on datacenter GPUs is where
> infinity is designed to win. Not measured here; that box wasn't on hand.

### What this implies for the ~40-min reindex

71k embeds at the current qwen3-0.6b/ollama rate (~10–20 c/s) ≈ 40–60 min —
matches the report. At bge-large/ollama's ~56 c/s the *same* 71k embeds ≈ 21 min,
**before** any concurrency or backend change. Model choice alone nearly halves it.

---

## §1 — Tier 0: the embed-model default (biggest measured win)

`DEX_EMBED_MODEL` defaults to the ollama-detected model, and the fallbacks point
at qwen3-family (`cmd/dex/env.go:38`, `main_clients.go`). The data says that
default is a throughput trap on ollama. Two moves, cheapest first:

- **Benchmark candidate embed models on the ollama path** (bge-m3, bge-large,
  nomic-embed-text, snowflake-arctic-embed) for chunks/s **and** retrieval
  quality via dex's existing eval harness (`cmd/dex/eval.go`,
  `internal/eval/`). Throughput is worthless if recall drops — this must be
  eval-gated, not vibes.
- If a faster model holds retrieval quality, **change the recommended default**
  and document the tradeoff. This is a defaults/docs change, no new subsystem.

This is Tier 0 because it dominates every other lever and needs no new
dependency — but it is **gated on the eval**, not a blind swap. Retrieval quality
is the product; throughput is the constraint.

## §2 — The graph-node second pass (~35k tiny embeds)

`NodesNeedingEmbed` → `em.Embed(nodeEmbedText…)` (`graphrefresh.go`,
`store_graph.go:1116`) re-embeds ~35k node signatures. Each input is a *trivial
synthetic string* — `nodeEmbedText(kind, name, qualifiedName)` = `"func
github.com/x/y.Foo"` — so its cost is ~100% per-request serving overhead. At
20 c/s, 35k nodes ≈ 29 min of the reported ~40. This pass is the fattest target
and it's independent of the backend question:

- **Real dedup question (its own spec):** node vectors and chunk vectors live in
  different spaces (synthetic signature vs. chunk body), so they aren't
  interchangeable as-is. But does a node need a *separate* embed at all? For a
  symbol that is also a chunk, its semantic neighbors could derive from the
  enclosing chunk vector instead of a fresh embed of a near-contentless string —
  potentially removing the 35k second pass outright. This changes what node KNN
  *means*, so it's a **separate issue**, not folded into #77. Flagged as the
  likely single biggest structural win for node-heavy repos.

> **Partly addressed in #91 (Tier A).** Investigation found a sharper problem
> than the first-index 35k: the re-embed gate was `vec_hash != content_hash`,
> but `content_hash` includes start/end line+byte spans while `nodeEmbedText`
> depends only on `kind`+`qualified_name`. So *every incremental reindex*
> re-embedded an identical signature string for every node below an edit point —
> recurring waste, not just a one-time cost. #91 re-keys the gate on the embed
> text (`vec_hash` now stores it; the `WHERE` reconstructs it in SQL), so a line
> shift no longer re-embeds. The first-index 35k remains (all nodes are new) —
> eliminating *that* needs the vector-reuse change above (chunk-vector reuse /
> chunk-KNN seeding), which alters NodeKNN semantics and stays deferred behind a
> retrieval eval (Tier B), consistent with §7's accept-the-one-time-cost verdict.

## §3 — The inert concurrency path (Tier 1, conditional win)

`newEmbedClient` wires `DEX_EMBED_CONCURRENCY` (default 4) into
`embed.NewWithConcurrency` (`main_clients.go:79`); `client.Embed` fans out up to
`conc` sub-batches **within one call**. But the chunk loop feeds it exactly one
client-batch:

```
internal/index/index.go:484   batchSize := ix.Embed.BatchSize()   // == client.Batch
                    …         embedAndUpsertBatch(ctx, batch)      // len == batchSize
internal/index/index.go:612   ix.Embed.Embed(ctx, texts)          // len == batchSize
```

→ the errgroup gets exactly one task; concurrency is dead in the chunk pass. (The
graph pass calls `Embed` with 256 texts (`graphrefresh.go`), so it *does* split
into concurrent sub-batches — the node pass already parallelizes, the larger
chunk pass does not.) The crash-survival design (embed-and-upsert one batch at a
time, cf. `interrupted_run_test.go`) is what serialized it.

**Fix (small, in-scope):** decouple the embed super-batch from the upsert
granularity — feed `Embed` `conc × BatchSize` texts, upserting per client-batch
as vectors return (preserving crash-survival), or run `embedAndUpsertBatch` in an
errgroup with `SetLimit(conc)`.

> **Implemented in #96.** The embed loop now iterates at a super-batch of
> `BatchSize × EmbedConcurrency` (new `Embedder.EmbedConcurrency()` method) and
> hands the whole super-batch to `Embed`, which fans out internally. Crash-survival
> is preserved at super-batch granularity (embed super-batch → upsert → next);
> `UpsertMany` is a per-row prepared stmt, so the wider batch has no SQL
> param-count limit. Failure-isolation coarsens from `BatchSize` to the super-batch
> — bounded and re-queued via content-sha next run.

**But it's a conditional win** (measured): +1.88× on qwen3 (GPU underused
single-stream), −23% on bge-large (already saturated). So it should be *engaged*
and *auto-tuned per backend*, not assumed positive. Pairs with §4.

## §4 — Per-backend batch/concurrency defaults (Tier 1)

`BatchSizeForVRAM` (`main_clients.go:78`; 8/64/256 for <4/4–16/>16 GB) is tuned
for a true-batching server and pessimizes ollama+qwen3 (its worst point is
batch 32). dex already detects ollama (`DetectOllama`, `main_clients.go:44`), so
branch on it:

- **ollama** → small batch (~8–16), concurrency on (helps the slow-single-stream
  models, neutral-to-mild elsewhere).
- **infinity/TEI/vLLM** → large batch, concurrency ~1 (batching already
  saturates; client concurrency was neutral/negative in the A/B).

Mostly a defaults + `docs/deployment.md` change; no new subsystem.

> **Implemented in #97.** `embedBackendDefaults` (`cmd/dex/main_clients.go`)
> branches on the `DetectOllama` result: auto-detected ollama → batch 16 +
> concurrency 4; any other (or explicit `DEX_EMBED_URL`) backend → VRAM-sized
> batch + concurrency 1. `DEX_EMBED_BATCH` / `DEX_EMBED_CONCURRENCY` still
> override.
>
> A reindex grid on qwen3-embedding:0.6b (6.8k chunks, this ollama) pinned the
> values and *revised the §4 hypothesis*: the win is **concurrency, not small
> batch**. Sequential throughput does climb as batch shrinks (7.8 c/s @128 →
> 14.7 c/s @8), but concurrency=4 flattens the batch-collapse entirely — batch
> 16 and 128 both hit the same ~16.7 c/s GPU-bound ceiling, and batch 8 is
> marginally *worse* (15.3). The RFC's headline "26.7 c/s @ batch 8" (single
> stream) did not reproduce. So ollama gets a *small-ish* batch (16) mainly to
> cap VRAM and protect a `conc=1` override, and concurrency 4 does the real
> work — the lever #96 made live.
>
> | batch | conc | embed (6.8k chunks) | c/s |
> |------:|-----:|--------------------:|----:|
> | 128 | 1 | 869.6s | 7.8 |
> | 8   | 1 | 463.0s | 14.7 |
> | 8   | 4 | 443.2s | 15.3 |
> | 16  | 4 | **407.3s** | **16.7** |
> | 128 | 4 | 406.5s | 16.7 |

## §5 — infinity as an opt-in embed backend (Tier 2, CUDA-only)

Compatibility is genuinely small — dex speaks the OpenAI embed shape, infinity
exposes `/embeddings`, and infinity v2 takes **repeated `--model-id`** so the
embed model can co-host on the existing reranker instance (mac launchd
`components/dex/index.yml`; Linux container `server.yml`), `DEX_EMBED_URL` →
that port. **But the A/B says don't do this on MPS** — it's slower. Scope it to
the CUDA server where torch batching wins, keep it opt-in, and require a reindex
on repoint (`EnsureEmbedModel` already forces reindex on model change; confirm it
trips on an endpoint swap that keeps the model name). No dex code change beyond
§4's branch + docs.

## §6 — Alternatives

| Backend | Same-model MPS speed | New process? | Verdict |
|---|---|---|---|
| **ollama** (llama.cpp/Metal) | **fastest measured** | already there | **keep as default**; fix model (§1) + concurrency (§3) |
| infinity (torch/MPS) | ~1.7× slower | no (reuse rerank instance) | **opt-in, CUDA-server only** (§5) |
| infinity (torch/CUDA) | not measured — likely wins | container already deployed | opt-in server lane |
| ONNX in-process (`-tags onnx`) | not measured | no daemon | keep as the no-daemon lane; don't expand |
| llama.cpp server batch mode | n/a | yes | no — worse than reusing infinity |

## Decision

- **Do first (Tier 0, eval-gated):** benchmark embed-model candidates on ollama
  for throughput **and** retrieval quality; if one dominates, change the default.
  Biggest measured lever, no new dependency.
- **Do (Tier 1, in-tree):** engage the dead concurrency path (§3) + per-backend
  batch/concurrency defaults (§4). Small, self-contained, auto-tuned.
- **Defer to its own issue (Tier ?):** graph-node embed elimination/reuse (§2) —
  likely the biggest structural win, but changes node-KNN semantics; own spec.
- **Do NOT:** adopt infinity for embedding on the Mac (measured slower), make it
  the default, or add a standalone embed daemon. Keep it opt-in and CUDA-scoped.
- **Accept-the-cost is legitimate:** first-index is paid once; the mtime
  `AfterIndex` incremental path covers steady state. For most repos this whole
  effort is moot — it earns its keep only on large first-indexes, and even there
  Tier 0 + Tier 1 get most of the win without a second serving dependency.

## Phasing

1. **Phase 1 — Tier 0 eval:** model-vs-model throughput + retrieval eval on the
   ollama path (`cmd/dex/eval.go`). Output: keep or change the default embed
   model. No code beyond a possible default flip.
2. **Phase 2 — Tier 1:** decouple embed super-batch from upsert granularity so
   concurrency engages; per-backend batch/concurrency defaults. Bench before/after
   on a real repo. Guard crash-survival with the existing
   `interrupted_run_test.go` / `reindex_race_test.go` patterns.
3. **Phase 3 — separate issue:** graph-node embed reuse (§2).
4. **Phase 4 (optional, docs):** infinity CUDA-server embed lane in
   `docs/deployment.md` + `components/dex` opt-in var.

## Risks

- **Model swap must not regress retrieval** — Tier 0 is eval-gated; throughput is
  the constraint, recall is the product.
- **Concurrency fix must preserve crash-survival** — upsert per client-batch, not
  per super-batch, so interrupted runs resume cleanly.
- **Backend swap mixes vector spaces** — reindex on `DEX_EMBED_URL` repoint;
  verify `EnsureEmbedModel` trips on same-name/different-endpoint.
- **Numbers are MPS-only, one box, contended** — the CUDA-server ordering is
  unmeasured and may favor infinity; don't over-generalize the Mac result to the
  server lane.
- **Scope discipline** — Tier 0 + Tier 1 are the commitment; §2 and §5 are
  explicitly separate issues, not smuggled into #77.
