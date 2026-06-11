#!/usr/bin/env bash
# A/B sweep: embed model × Matryoshka dim truncation.
# Runs dex reindex + dex bench eval for each (MODEL, DIM) combination and
# collects NDCG@10 / Recall@10 / MRR into results/embed-ab.tsv.
#
# Usage:
#   ./scripts/sweep-embed-ab.sh <project-path>
#
# Env overrides:
#   MODELS     space-separated list of model names to test (default: below)
#   DIMS       space-separated list of dim caps to test, 0 = full (default: below)
#   GOLDEN     path to golden.json (default: <project>/benchmark/eval/golden.json)
#   RESULTS    output TSV path (default: benchmark/eval/embed-ab.tsv)
#
# Prerequisites: dex must be installed (mooncake task install) and the
# embedding endpoint reachable (DEX_EMBED_URL / ollama).

set -euo pipefail

PROJECT="${1:?usage: $0 <project-path>}"

MODELS="${MODELS:-qwen3-embedding:4b qwen3-embedding:0.6b}"
DIMS="${DIMS:-0 512 1024}"
GOLDEN="${GOLDEN:-$PROJECT/benchmark/eval/golden.json}"
RESULTS="${RESULTS:-$(dirname "$0")/../benchmark/eval/embed-ab.tsv}"

mkdir -p "$(dirname "$RESULTS")"

# Stop dex serve so it doesn't compete for the embed endpoint during the sweep.
# Restart it (same command line) on exit, whether the sweep succeeds or fails.
DEX_SERVE_PID=$(pgrep -f 'dex serve' | head -1 || true)
DEX_SERVE_CMD=""
if [[ -n "$DEX_SERVE_PID" ]]; then
  DEX_SERVE_CMD=$(cat /proc/"$DEX_SERVE_PID"/cmdline | tr '\0' ' ' | sed 's/ $//')
  echo "==> stopping dex serve (pid $DEX_SERVE_PID) to free embed endpoint" >&2
  kill "$DEX_SERVE_PID"
  # Give it a moment to release the embed connection.
  sleep 2
fi

restore_dex() {
  # Unload test model; let ollama reload the right one lazily on next use.
  ollama stop qwen3-embedding:4b  2>/dev/null || true
  ollama stop qwen3-embedding:0.6b 2>/dev/null || true
  # Restore index to original model/dim so dex serve picks up cleanly.
  if [[ -n "${ORIG_MODEL:-}" ]]; then
    echo "==> restoring index: model=$ORIG_MODEL dim=${ORIG_DIM:-0}" >&2
    DEX_EMBED_MODEL="$ORIG_MODEL" DEX_EMBED_DIM="${ORIG_DIM:-0}" dex reindex "$PROJECT" >/dev/null 2>&1 || true
  fi
  if [[ -n "$DEX_SERVE_CMD" ]]; then
    echo "==> restarting dex serve" >&2
    nohup bash -c "$DEX_SERVE_CMD" </dev/null >/tmp/dex-serve-restart.log 2>&1 &
  fi
}
trap restore_dex EXIT

# Evict large chat model so it can't load and steal VRAM during the sweep.
DEX_CHAT_MODEL_RUNNING=$(ollama ps 2>/dev/null | awk 'NR>1 && /qwen3/ && !/embed/ {print $1}' | head -1 || true)
if [[ -n "$DEX_CHAT_MODEL_RUNNING" ]]; then
  echo "==> evicting chat model $DEX_CHAT_MODEL_RUNNING from VRAM" >&2
  ollama stop "$DEX_CHAT_MODEL_RUNNING" 2>/dev/null || true
fi

# Record baseline model/dim so we can restore on exit.
ORIG_MODEL="${DEX_EMBED_MODEL:-qwen3-embedding:4b}"
ORIG_DIM="${DEX_EMBED_DIM:-0}"

# Header
echo -e "model\tdim\tndcg10\trecall10\tmrr" > "$RESULTS"

for model in $MODELS; do
  for dim in $DIMS; do
    label="${model}@${dim}"
    echo "==> $label" >&2

    export DEX_EMBED_MODEL="$model"
    export DEX_EMBED_DIM="$dim"

    # Rebuild index with this model+dim combo.
    echo "  reindexing..." >&2
    dex reindex "$PROJECT" >/dev/null

    # Score against the committed golden set.
    echo "  evaluating..." >&2
    json=$(dex bench eval "$PROJECT" --golden "$GOLDEN" --output json)

    ndcg=$(echo "$json"   | python3 -c "import json,sys; d=json.load(sys.stdin); print(round(d['mean_ndcg'],4))")
    recall=$(echo "$json" | python3 -c "import json,sys; d=json.load(sys.stdin); print(round(d['mean_recall'],4))")
    mrr=$(echo "$json"    | python3 -c "import json,sys; d=json.load(sys.stdin); print(round(d['mrr'],4))")

    echo -e "${model}\t${dim}\t${ndcg}\t${recall}\t${mrr}" | tee -a "$RESULTS"
  done
done

echo "" >&2
echo "Results written to $RESULTS" >&2
column -t -s $'\t' "$RESULTS"
