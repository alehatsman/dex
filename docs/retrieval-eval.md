# Retrieval evaluation

dex ships an offline retrieval eval harness (`dex bench eval`) that scores the
live `Search` path against a golden set of `(query → relevant files)` pairs
mined from a repo's own git history, reporting **NDCG@10, Recall@10,
RecallPool@candidateK, MRR**, broken down **by query type** (nl / symbol /
architecture). It gates retrieval regressions and provides the measurement
layer for tuning fusion, lanes, embedders, and index features.

`RecallPool@candidateK` is the recall measured at the full candidate-pool depth
(`k*5`, min 30) the fusion/rerank stage sees — the **ceiling** top-k Recall
can't exceed. The gap `RecallPool − Recall` separates a *ranking* failure (the
file was in the pool, fusion/rerank buried it) from a *retrieval* failure (it
never made the pool). The per-type breakdown catches a per-bucket regression an
aggregate mean would hide.

See `benchmark/eval/README.md` for the golden-set generation mechanics. This
doc covers **what the metric does and does not measure**, and **how to A/B a
change** without fooling yourself.

## What it measures

- **Golden set:** each non-merge commit touching 1–N source files becomes one
  labeled query. **Query** = the commit subject (conventional-commit prefix
  stripped). **Relevant** = the files that commit touched (still on disk).
- **Runner:** embeds each query, runs the live fused `Search`, collapses hits
  to a ranked list of unique source files, scores NDCG@10 / Recall@10 / MRR
  against the relevant set. `git_commit` chunks are always dropped (the query
  *is* the commit subject — the matching commit chunk would be a trivial leak).

```sh
dex bench eval .                                 # print the metrics table
dex bench eval . --check benchmark/eval/baseline.json   # regression gate (>0.02 drop fails)
mooncake task eval                               # same, as a task
```

## What it does NOT measure — the relevance-model blind spot

The relevance model is **"files a commit touched, queried by the commit
subject."** Those touched files are overwhelmingly the **direct** semantic /
BM25 matches for the subject text. Consequences:

- ✅ It measures **direct-match retrieval** (dense + lexical fusion, rerank,
  path/definition boosts) well.
- ❌ It is **insensitive to structural-neighbor lanes.** The graph-proximity
  lane only re-weights *neighbors* (callers/callees) of matched files — which
  are *other* files, not the touched set — so it can't change whether the
  relevant files land in the top-10.

This is not hypothetical. The #248 graph hop-decay (γ^hop) A/B swept
γ ∈ {0.4…0.8} against flat-0.5× and came back a **null result** (NDCG/Recall/MRR
all within ±0.001) — precisely because this instrument doesn't probe what graph
expansion is for. The literature's graph gains (e.g. RepoGraph, +32.8%) are on
SWE-bench **resolve-rate** ("find every file you must edit together"), a
different task than commit→file retrieval.

**Implication:** graph-lane / structural-retrieval work needs a different
instrument — the **blast-radius golden set** (`--mode blast-radius`, below).
Don't claim a structural-retrieval win on the git-history golden set.

## Blast-radius mode (`--mode blast-radius`)

A second golden flavor that probes **structural / "what changes with this?"**
relevance. For each multi-file commit, every touched code file becomes an
**anchor**: the query is a code excerpt of that anchor's *current* content and
the relevant set is the **other** files co-changed in the same commit. The
anchor itself is excluded from the ranked results (it's the given). This
rewards retrieving structurally-coupled files that are *not* direct lexical
matches — exactly the graph lane's job.

```sh
dex bench eval . --gen --mode blast-radius      # writes benchmark/eval/blast-radius.json
dex bench eval . --golden benchmark/eval/blast-radius.json   # score it
```

Caveat: the query excerpt includes the file's import lines, so co-changed files
that the anchor *imports* can still surface as direct matches — the instrument
is sensitive to the graph lane but not purely structural. Use it for *relative*
A/B of structural-retrieval changes (#248), not as an absolute SOTA number.

### What it found (the #248 graph-lane verdict)

Re-running the #248 flat-0.5 vs γ^hop A/B on this instrument (596 queries,
rerank off) came back **null again** — NDCG/Recall/MRR all within ±0.0007:

| arm        | NDCG@10 | Recall@10 | MRR    |
|------------|---------|-----------|--------|
| flat-0.5   | 0.3423  | 0.4188    | 0.4379 |
| γ^hop 0.4  | 0.3422  | 0.4182    | 0.4379 |
| γ^hop 0.6  | 0.3425  | 0.4189    | 0.4377 |

But a ranked-list diff shows the instrument **is** sensitive: γ0.6 reorders the
top-10 on **41/596** queries. The null is not blindness — it's leverage. Of
those 41, only **7** move a *relevant* file's rank, and every such move is **±1
rank and they cancel** (e.g. `server_session` 4→3 helps, `server.go` 9→drops-out
hurts). So the graph lane engages but barely shifts relevant files.

Root cause: dense+BM25 already rank the co-changed files, and the graph lane's
RRF contribution (k=60, per-neighbor weight ~0.5–0.6) is too small to reorder
them by more than one slot. **The bottleneck is the graph lane's overall fusion
weight, not the hop-decay shape (γ).** Tuning γ tunes the decay of a lane that
barely registers. Making graph proximity measurably improve retrieval would
require raising the lane's weight so it competes with dense+BM25 — a different
change than #248's hop-decay tuning, which is parked on this finding.

## Multi-repo corpus (`dex bench corpus`)

Both golden flavors above are mined from **dex's own** git history — so every
tuning decision is fit to a single Go codebase. A fusion-weight or chunker change
that wins on dex's idioms can silently regress a Python/TS/Rust repo. The
**corpus** generalizes the gate to a set of **pinned real-world repos** across
languages (#278).

`benchmark/corpus/repos.yml` lists repos pinned at a release tag's commit; the
runner fetches each at its pin, indexes it, and scores the live `Search` path
against every declared golden set — curated query sets (`queries/<name>.json`,
`eval.GoldenSet` shape) and/or `gen` auto-labels (the same git-history /
blast-radius generators, run on each repo). It reuses `internal/eval` wholesale.

```sh
dex bench corpus run                                   # whole corpus (fetch+index on first run)
dex bench corpus run --smoke --repos flask,gin          # fast: first curated set per repo
dex bench corpus run --check benchmark/corpus/baseline.json   # gate: fail on ANY per-cell >0.02 drop
mooncake task corpus                                    # same, as a task
```

The gate is **per (repo, set) cell**, not the aggregate mean — a single-repo
regression can't hide behind a steady average. Curated and generated sets are
reported as separate rows (they measure different things; don't average across).
Like `dex bench eval`, it needs a live embed endpoint + network and is a
local/main_pc gate, not container-CI. See `benchmark/corpus/README.md` for
adding a repo and authoring query sets.

## Fusion calibration — the #317 verdict

The dense+BM25 lanes are merged by one of two strategies (`DEX_FUSION_MODE`):

- **FusionRRF** — Reciprocal Rank Fusion: combines lanes by *rank position*
  only (`Σ weight/(60+rank)`), discarding score magnitude. Scale-free, robust.
- **FusionLinear** — convex combination on min-max normalised scores:
  `α·dense + (1−α)·bm25`. Magnitude-aware; α (`DEX_FUSION_ALPHA`) is the dense
  weight.

An earlier result ("+17% NDCG at α=0.2") was measured on dex's own golden set —
the same queries used to pick α, so train == test. #317 re-ran this as a
**leave-one-repo-out (LORO)** sweep over the corpus: for each held-out repo, the
α is chosen on the *other* repos, then scored on the held-out one
(`benchmark/corpus/sweep-lane-weights.sh` + `analyze-sweep.py`).

**Verdict:** FusionLinear with **α=0.7** wins, robustly — selected in **all 5
LORO folds** (not contaminated), +3.3% NDCG / +3pts Recall vs RRF on held-out
repos, and **+145% NDCG over RRF on dex-self**. The α curve is a clean interior
optimum: it climbs from α=0.1, peaks at 0.7, and falls back toward pure-dense
(α=1.0) — α=0.2 was simply far too BM25-heavy. This is now the **default**
(`fusionMode`/`fusionAlpha` in `cmd/dex/main.go`); `DEX_FUSION_MODE=rrf` reverts.

Two findings bound what the sweep could tune:

- **`GraphLaneWeight` is inert** under the production (rerank-on) path — gw ∈
  {1,4,8} produced identical NDCG (±0.001) across all sets. The cross-encoder
  reranker reorders the candidate pool *after* the graph lane, so the weight
  only affects pool membership, which the dense+BM25 lanes already determine.
  This is the same wall #248 hit; the graph lane can't be calibrated here while
  the reranker dominates. Left at 1.0.
- **`RecallPool` barely moves with α** (~0.85 corpus) — α reorders *within* the
  pool, it doesn't change which docs the reranker sees. The win is a
  ranking-order win, consistent with the reranker-pool model above.

## A/B-testing a change

Run the *same* golden set under both arms, changing one variable. Disable
rerank (`DEX_DISABLE_RERANK=1`) when you want to isolate the **fusion** layer —
rerank reorders the top pool and can mask a fusion-lane change.

**Query-time change (fusion weight, lane, rerank):** one index, two binaries or
two env settings:

```sh
DEX_DISABLE_RERANK=1 DEX_GRAPH_GAMMA=0.6 dex bench eval . --output json
```

**Index-level change (e.g. chunk summaries on/off):** you must reindex under
each arm — and pass `--keep-summaries`. By default the runner drops
summary-kind chunks from the ranked file list (to keep the git-history scoring
code-focused). If you're A/B-ing a feature that *produces* summary chunks, that
filter hides exactly the effect you're measuring: a file surfaced via its
summary would be invisible, making the feature look like a no-op.

```sh
# arm A: summaries on
dex index .                                      # with summary drain enabled
dex bench eval . --keep-summaries --output json > /tmp/with-summaries.json

# arm B: summaries off  (reindex without summary chunks)
DEX_AUTO_SUMMARIZE=off dex reindex .
dex bench eval . --keep-summaries --output json > /tmp/no-summaries.json
```

`--keep-summaries` keeps the unconditional `git_commit` filter (summaries carry
no commit-subject leak — they derive from file content), so it's safe to enable
for any A/B.

### Reading the result

- Differences within **±0.002** on a ~300-query set are noise — don't ship a
  default change on that basis (it's complexity without measured benefit).
- A real effect moves a metric by a clear margin across the whole arm and is
  stable across reruns (the golden set and search are deterministic; only the
  embed endpoint's availability varies).
