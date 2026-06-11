#!/usr/bin/env bash
# sweep-lane-weights.sh — fusion calibration sweep over the multi-repo corpus (#317).
#
# Runs `dex bench corpus run` once per fusion setting over ALL corpus repos and
# saves the per-repo cells as JSON. Each setting is run once; leave-one-repo-out
# (LORO) is then pure post-processing over the per-repo cells (see analyze-sweep.py)
# — no per-fold re-runs needed, since a single run already yields one cell per repo.
#
# This de-contaminates the FusionLinear α decision: the original "+17% NDCG"
# was measured on dex's own golden set (tuning == measurement). Here every repo
# is held out in turn against settings chosen on the others.
#
# Usage:
#   benchmark/corpus/sweep-lane-weights.sh [out_dir]
#
# Env passthrough: DEX_* (embed/rerank URLs etc.) are inherited. The sweep only
# overrides DEX_FUSION_MODE / DEX_FUSION_ALPHA / DEX_GRAPH_LANE_WEIGHT per cell.
#
# Re-running is idempotent: a setting whose JSON already exists is skipped, so
# an interrupted sweep resumes where it left off. Delete out_dir to start fresh.
set -euo pipefail

OUT_DIR="${1:-benchmark/corpus/sweep-results}"
mkdir -p "$OUT_DIR"

# One run per setting. Tag encodes the setting so analyze-sweep.py can parse it.
#   mode=rrf                       — incumbent default
#   mode=linear alpha=0.1..1.0     — convex-combination fusion, dense weight α
#   graph weight probe (gw=1,4)    — documents whether the graph lane moves the
#                                     metric at all under the production (rerank-on) path
run_setting() {
  local tag="$1"; shift
  local out="$OUT_DIR/$tag.json"
  if [[ -s "$out" ]]; then
    echo "skip  $tag (exists)"
    return
  fi
  echo "run   $tag : $*"
  env "$@" dex bench corpus run --output json >"$out" 2>"$OUT_DIR/$tag.err" || {
    echo "FAIL  $tag (see $OUT_DIR/$tag.err)" >&2
    rm -f "$out"
    return 1
  }
}

# --- Fusion mode / alpha dimension (the core de-contamination sweep) ----------
run_setting "rrf"           DEX_FUSION_MODE=rrf    DEX_GRAPH_LANE_WEIGHT=1.0
for a in 0.1 0.2 0.3 0.5 0.7 1.0; do
  run_setting "linear_a${a}" DEX_FUSION_MODE=linear DEX_FUSION_ALPHA="$a" DEX_GRAPH_LANE_WEIGHT=1.0
done

# --- GraphLaneWeight probe (per fusion mode) ----------------------------------
# Documents the inertness finding: under the reranker-on production path the
# graph lane only affects rerank-pool membership, so the metric is expected to
# be flat across weights. Kept in the sweep so the claim is reproducible.
run_setting "rrf_gw4"        DEX_FUSION_MODE=rrf    DEX_GRAPH_LANE_WEIGHT=4.0
run_setting "rrf_gw8"        DEX_FUSION_MODE=rrf    DEX_GRAPH_LANE_WEIGHT=8.0

echo "done. results in $OUT_DIR/  →  analyze with benchmark/corpus/analyze-sweep.py $OUT_DIR"
