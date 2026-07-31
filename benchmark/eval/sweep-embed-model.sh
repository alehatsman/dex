#!/usr/bin/env bash
# Embed-model A/B sweep (#90): reindex a repo with each candidate embed model,
# time the reindex, and score retrieval quality against the committed golden set.
#
# Unlike sweep-graph-gamma.sh (a runtime knob, no reindex), the embed model is
# baked into the vectors, so each candidate needs a FULL reindex — this clobbers
# the target repo's index. Run it against a throwaway worktree, NOT your live
# index. The script restores the default model at the end.
#
# Usage:  ./sweep-embed-model.sh /path/to/worktree "qwen3-embedding:0.6b bge-large ..."
# Requires: a live ollama (each model must be `ollama pull`ed first) + `dex`.
set -euo pipefail

REPO="${1:?usage: sweep-embed-model.sh <repo-dir> [\"model1 model2 ...\"]}"
MODELS="${2:-qwen3-embedding:0.6b bge-large}"
DEFAULT_MODEL="qwen3-embedding:0.6b"   # restored on exit
K="${K:-10}"

printf 'model\tindex_s\tndcg%s\trecall%s\tmrr\n' "$K" "$K"
for m in $MODELS; do
  t0=$(date +%s)
  DEX_EMBED_MODEL="$m" dex reindex "$REPO" >/dev/null 2>&1
  t1=$(date +%s)
  # dex bench eval reads the index-recorded model; --allow-stale-golden so a
  # golden mined at an older HEAD still scores (comparable across models here).
  read -r ndcg recall mrr < <(
    dex bench eval "$REPO" --k "$K" --allow-stale-golden --output json 2>/dev/null \
      | python3 -c 'import sys,json;d=json.load(sys.stdin);print(d["mean_ndcg"],d["mean_recall"],d["mrr"])'
  )
  printf '%s\t%s\t%.4f\t%.4f\t%.4f\n' "$m" "$((t1-t0))" "$ndcg" "$recall" "$mrr"
done

echo "# restoring default model ($DEFAULT_MODEL)…" >&2
DEX_EMBED_MODEL="$DEFAULT_MODEL" dex reindex "$REPO" >/dev/null 2>&1
echo "# done. Record results in benchmark/eval/embed-ab.tsv." >&2
