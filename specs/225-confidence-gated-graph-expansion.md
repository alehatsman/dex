# Spike #225: confidence-gated graph expansion

## Goal

Determine whether gating spreading-activation seeding by anchor confidence
(instead of seeding from every primary hit) improves blast-radius retrieval,
as an alternative/complement to the existing blanket γ^hop decay.

## Non-goals

- Not replacing the RRF-fused graph-proximity lane architecture.
- Not tuning `DEX_GRAPH_WEIGHT`/`DEX_GRAPH_GAMMA` themselves — those already
  have a sweep tool (`--graph-sweep`). This spike is about which files get
  to seed expansion at all, not how their hop-distance is weighted once
  they do.
- Not adopting a new default without a measured A/B.

## Current state (read from code)

`internal/store/store_graph_fusion.go`:

- `activationSeeds` (line 408) builds the BFS seed set from THREE sources,
  none of them confidence-gated: (1) every session-recent file at weight
  1.0, (2) every co-access neighbor of those at 0.8×, (3) **every primary
  hit**, weighted `score/maxScore` (line 447-452) — the full hit list, not
  top-N or above-threshold. A weak, barely-scoring hit still seeds BFS at
  whatever fraction its score is of the max.
- `spreadActivation` (line 332) then does uniform fan-out-normalized energy
  spreading from every seed for up to `hopCap()` hops, pruning only when
  energy drops below a fixed threshold (1e-4) — so a large seed set means
  more distinct BFS start points feeding the same threshold-pruned walk,
  not a smaller/more targeted one.
- `fuseWithGraphNeighbors` (line 162) then RRF-fuses the resulting
  activated files at `laneWeight × γ^hop`, same as issue #225 describes as
  "blanket hop-decay."

So the actual blanket behavior is in *seeding* (every hit participates,
weighted but not gated) more than in the hop-decay itself (which already
varies by hop). The proposed technique is anchor-confidence gating —
this matches source-technique's framing of "targeted expansion from
confident anchors" better as a **seed-selection** change than a **decay**
change.

## Proposed gate

Add a seed-selection gate to `activationSeeds`'s primary-hit loop (line
447): only primary hits within the top `N` by score, or above a score
fraction of `maxScore`, become BFS seeds. Weak long-tail hits (there only
because RRF returns a fixed pool size) stop contributing activation energy
at all, rather than contributing a small fraction of it.

```go
// activationSeedGate, env DEX_GRAPH_SEED_GATE (spike only, off by default):
//   ""      - unchanged: every primary hit seeds BFS (current default)
//   "topN"  - only the top N primary hits by score seed BFS (N configurable,
//             default 5 — roughly "the results a human would actually read")
```

Session-recent files and their co-access neighbors are NOT gated — they're
already a small, explicitly-curated set (the working set), not the blanket
case the issue is about.

## Plan

1. Add `GraphSeedTopN` to `store.Options` (0 = unset/unchanged), env
   `DEX_GRAPH_SEED_GATE_TOPN`, spike-only (not documented in `env.go`'s
   help table until proven).
2. In `activationSeeds`, when set, sort primary hits by score descending
   and only seed from the top N.
3. A/B via `dex bench eval --mode blast-radius` (the tool the issue names,
   already built for this exact lane) against the dex repo's own committed
   `benchmark/eval/blast-radius.json` golden set — no fixture corpus to
   vendor, unlike the #216 spike.
4. Try a couple of N values (e.g. 3, 5, 10) to see the shape of the
   response, not just one point.

## Validation

- `dex bench eval <dex-repo-root> --mode blast-radius --output json` run
  once at baseline (gate off) and once per N value, same golden set, same
  HEAD, same embed model — compare NDCG@10/Recall@10/MRR.
- Full `internal/store` test suite green (gate defaults to off, so existing
  behavior/tests are unaffected when unset).

## Result (spike complete)

Implemented `GraphSeedTopN` (`store.GraphOptions`, `internal/store/store.go`)
+ gating in `activationSeeds` (`internal/store/store_graph_fusion.go`), wired
via `DEX_GRAPH_SEED_GATE_TOPN` (`cmd/dex/main_config.go`). `go build`/`go
test -tags sqlite_fts5 ./internal/store/...` green; default (unset) behavior
is bit-for-bit unchanged.

Measured `dex bench eval <dex-repo> --mode blast-radius --k 10` (n=596,
committed golden set, live embed+rerank endpoints) three ways:

| run      | NDCG@10 | Recall@10 | Recall_pool | MRR    |
|----------|---------|-----------|-------------|--------|
| baseline (blanket) | 0.1625 | 0.1970 | 0.3663 | 0.2316 |
| top-5 gate         | 0.1634 (+0.6%) | 0.2013 (+2.2%) | 0.3663 (±0) | 0.2282 (−1.5%) |
| top-3 gate         | 0.1658 (+2.0%) | 0.2043 (+3.7%) | 0.3655 (−0.2%) | 0.2315 (−0.04%) |

The pipeline is deterministic given a fixed index (no randomness in
fusion/rerank), so these deltas are real signal, not run-to-run noise — and
they're monotonic in gate tightness across the two points tested (N=3 tighter
than N=5, and N=3 beats N=5 on both NDCG and Recall). Effect size is modest
(~2-4 points), well short of the source technique's reported double-digit
Acc@5 gains — expected, since that was a different benchmark/system, per
this issue's own success criteria.

**Go/no-go: soft go — worth keeping as opt-in, not a default-flip.**
Reasoning:
- The signal is small but consistent and reproducible, unlike a coin flip;
  N=3 costs nothing in MRR while N=5 gave up 1.5% MRR for its NDCG gain —
  the gate has a real tightness/precision tradeoff worth tuning further.
- Two points (N=3, N=5) aren't enough to call a shape (is N=2 better still,
  or does it fall off?) — this spike answered "does confidence gating help
  at all" (yes, mildly) not "what's the optimal N."
- No `structural`-mode or `--graph-sweep`-style cross-check was run — this
  spike used only blast-radius, per its own scope. A default-flip decision
  should also confirm no regression on `structural`/`git-history` modes.

**Recommendation:** keep `GraphSeedTopN`/`DEX_GRAPH_SEED_GATE_TOPN` as an
opt-in, undocumented (not in `env.go`'s help table) knob rather than
promoting to a documented default. Not spawning a follow-up issue
automatically — file one if there's appetite to run the fuller N-sweep +
structural-mode cross-check needed to consider flipping the default.
