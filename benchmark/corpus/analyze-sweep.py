#!/usr/bin/env python3
"""analyze-sweep.py — LORO analysis of the fusion calibration sweep (#317).

Reads the per-setting corpus JSONs written by sweep-lane-weights.sh and prints:
  1. Aggregate table — grand-mean NDCG/Recall/RecallPool per setting across repos.
  2. Per-repo NDCG matrix — setting x repo.
  3. Leave-one-repo-out — for each held-out repo, the setting that wins on the
     OTHER repos, and that setting's score on the held-out repo. If the same
     setting wins every fold, the choice is robust (not train/test contaminated).

Stdlib only. Usage: analyze-sweep.py [results_dir]
"""
import glob
import json
import os
import sys
from collections import defaultdict

results_dir = sys.argv[1] if len(sys.argv) > 1 else "benchmark/corpus/sweep-results"

# setting -> repo -> {ndcg, recall, recall_pool} (mean over that repo's query sets)
data: dict[str, dict[str, dict[str, float]]] = {}
for path in sorted(glob.glob(os.path.join(results_dir, "*.json"))):
    tag = os.path.splitext(os.path.basename(path))[0]
    with open(path) as f:
        cells = json.load(f)["cells"]
    by_repo = defaultdict(list)
    for c in cells:
        by_repo[c["repo"]].append(c["report"])
    data[tag] = {}
    for repo, reps in by_repo.items():
        n = len(reps)
        data[tag][repo] = {
            "ndcg": sum(r["mean_ndcg"] for r in reps) / n,
            "recall": sum(r["mean_recall"] for r in reps) / n,
            "recall_pool": sum(r.get("mean_recall_pool", 0.0) for r in reps) / n,
        }

if not data:
    sys.exit(f"no *.json in {results_dir} — run sweep-lane-weights.sh first")

settings = list(data.keys())
repos = sorted({r for s in data.values() for r in s})


def grand(tag, metric):
    vals = [data[tag][r][metric] for r in repos if r in data[tag]]
    return sum(vals) / len(vals) if vals else float("nan")


# 1. Aggregate -----------------------------------------------------------------
print("## Aggregate (grand mean across repos)\n")
print("| setting | NDCG | Recall | RecallPool |")
print("|---------|------|--------|------------|")
for tag in sorted(settings, key=lambda t: -grand(t, "ndcg")):
    print(f"| {tag} | {grand(tag,'ndcg'):.3f} | {grand(tag,'recall'):.3f} | {grand(tag,'recall_pool'):.3f} |")

# 2. Per-repo NDCG matrix ------------------------------------------------------
print("\n## Per-repo NDCG\n")
print("| setting | " + " | ".join(repos) + " |")
print("|" + "---|" * (len(repos) + 1))
for tag in settings:
    row = " | ".join(f"{data[tag].get(r,{}).get('ndcg',float('nan')):.3f}" for r in repos)
    print(f"| {tag} | {row} |")

# 3. Leave-one-repo-out --------------------------------------------------------
print("\n## Leave-one-repo-out (NDCG)\n")
print("| held-out repo | best-on-others | held-out NDCG @ that setting | held-out's own best |")
print("|---|---|---|---|")
fold_winners = []
for held in repos:
    others = [r for r in repos if r != held]
    # setting that maximizes mean NDCG over the OTHER repos
    best = max(settings, key=lambda t: sum(data[t][o]["ndcg"] for o in others if o in data[t]) / len(others))
    held_at_best = data[best].get(held, {}).get("ndcg", float("nan"))
    own_best = max(settings, key=lambda t: data[t].get(held, {}).get("ndcg", -1))
    fold_winners.append(best)
    flag = "" if best == own_best else f"  (own best: {own_best})"
    print(f"| {held} | {best} | {held_at_best:.3f} | {own_best}{flag} |")

consensus = set(fold_winners)
print()
if len(consensus) == 1:
    print(f"**Robust:** every fold selects `{fold_winners[0]}` — the choice is not train/test contaminated.")
else:
    print(f"**Split:** fold winners differ ({sorted(consensus)}) — no single setting dominates; report per-repo.")
