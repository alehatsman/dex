# Retrieval evaluation

dex ships an offline retrieval eval harness (`dex bench eval`) that scores the
live `Search` path against a golden set of `(query → relevant files)` pairs
mined from a repo's own git history, reporting **NDCG@10, Recall@10, MRR**. It
gates retrieval regressions and provides the measurement layer for tuning
fusion, lanes, embedders, and index features.

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
instrument — a **co-change / blast-radius golden set** (query = one touched
file, relevant = the *other* files co-changed in the same commit). Tracked as a
separate eval child of the search epic. Don't claim a structural-retrieval win
on the git-history golden set.

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
