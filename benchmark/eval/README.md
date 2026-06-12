# Code-retrieval eval

Offline regression gate for the dex `Search` path. Scores retrieval quality
against a **golden set** of `(query → relevant files)` pairs mined from this
repo's own git history, reporting the standard IR metrics **NDCG@10,
Recall@10, MRR**.

> For the methodology — what this instrument does and does **not** measure
> (its structural-neighbor blind spot), and how to A/B a change without fooling
> yourself — see [`docs/retrieval-eval.md`](../../docs/retrieval-eval.md).

## How the golden set is built

Each non-merge commit that touches between 1 and `--max-files` (default 5)
source files becomes one labeled query:

- **query** = the commit subject with its conventional-commit prefix stripped
  (`feat(mcp): maintenance mode stub` → `maintenance mode stub`). Stripping the
  prefix removes the `scope` token so it can't act as an unfair lexical anchor
  for BM25.
- **relevant files** = the source files that commit touched (still present on
  disk; non-code files filtered out).

This is reproducible from `git log` at a fixed HEAD, so the query set — and
therefore the metrics — are stable across runs.

## Usage

```sh
# (re)generate the golden set from git history
dex bench eval . --gen                       # writes benchmark/eval/golden.json

# score the live index and print the metrics table
dex bench eval . --k 10

# regression gate: fail if any metric drops >0.02 vs the baseline
dex bench eval . --check benchmark/eval/baseline.json
mooncake task eval                           # same, wrapped as a task

# refresh the committed baseline after an intentional improvement
dex bench eval . --output json > benchmark/eval/baseline.json
```

Requires a built `dex` on PATH, a live embed endpoint (`DEX_EMBED_URL` /
ollama) and an index for the repo (`dex index .`). Because of the embed
dependency this is a local / main_pc gate, **not** a container-CI step.

## Files

- `golden.json` — committed labeled query set (regenerate with `--gen`).
- `baseline.json` — committed metric baseline the `--check` gate compares against.
