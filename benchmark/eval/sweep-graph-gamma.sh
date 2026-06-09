#!/usr/bin/env bash
# γ-sweep for the graph-proximity hop-decay lane (#248).
#
# Scores the live Search path against the committed golden set across a range
# of DEX_GRAPH_GAMMA values, printing NDCG@10 / Recall@10 / MRR per γ so the
# best can be picked empirically and the gain proven vs the flat-0.5× lane.
#
# Prereqs (this is a GPU/embed-endpoint job — run when the summary drain is idle):
#   - dex built from this branch and on PATH (mooncake task install)
#   - a live embed endpoint (DEX_EMBED_URL / ollama)
#   - an index for the target repo (dex index <repo>)
#
# Usage:  benchmark/eval/sweep-graph-gamma.sh [repo-path] [golden.json]
set -euo pipefail

REPO="${1:-.}"
GOLDEN="${2:-benchmark/eval/golden.json}"
GAMMAS=(0.4 0.5 0.6 0.7 0.8)

printf '%-7s  %-9s  %-9s  %-9s\n' gamma NDCG@10 Recall@10 MRR
printf '%-7s  %-9s  %-9s  %-9s\n' ------- --------- --------- ---------

for g in "${GAMMAS[@]}"; do
  json="$(DEX_GRAPH_GAMMA="$g" dex bench eval "$REPO" --golden "$GOLDEN" --k 10 --output json 2>/dev/null)"
  ndcg="$(printf '%s' "$json"  | python3 -c 'import json,sys;print("%.4f"%json.load(sys.stdin)["mean_ndcg"])')"
  recall="$(printf '%s' "$json" | python3 -c 'import json,sys;print("%.4f"%json.load(sys.stdin)["mean_recall"])')"
  mrr="$(printf '%s' "$json"   | python3 -c 'import json,sys;print("%.4f"%json.load(sys.stdin)["mrr"])')"
  printf '%-7s  %-9s  %-9s  %-9s\n' "$g" "$ndcg" "$recall" "$mrr"
done

echo
echo "Baseline (flat-0.5× lane) for comparison: benchmark/eval/baseline.json"
echo "After picking γ: set defaultGraphGamma, refresh baseline.json, open the PR."
