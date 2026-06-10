# Multi-repo retrieval corpus

A cross-language retrieval benchmark for the dex `Search` path. Where
`dex bench eval` (see [`../eval/README.md`](../eval/README.md)) scores **only
this repo's** git history, the corpus scores a set of **pinned real-world repos**
across several languages — so retrieval tuning isn't overfit to dex's own Go
codebase (#278, epic #246).

> Methodology — what retrieval eval does and does **not** measure, and how to
> A/B a change — is in [`../../docs/retrieval-eval.md`](../../docs/retrieval-eval.md).

## How it works

`benchmark/corpus/repos.yml` lists repos pinned at a release tag's commit. For
each repo the runner:

1. **Fetches** it at its pinned commit into a cache (`<index-base>/corpus/<name>@<sha>`),
   using a blob-filtered partial clone (full history for auto-labels, blobs lazy).
2. **Indexes** it (only if not already indexed).
3. **Scores** the live `Search` path against every declared golden set, reporting
   NDCG/Recall/MRR per `(repo, set)`.

Each repo declares its query sources (≥1):

- **`query_sets`** — committed, hand-curated `eval.GoldenSet` JSON (NL question →
  relevant files), under `queries/<name>.json`. Paths are relative to `repos.yml`.
  These are the high-signal labels.
- **`gen`** — auto-labels mined from the repo's own history at the pin (free,
  noisier): `git_history` (commit subject → touched files) and `blast_radius`
  (anchor excerpt → co-changed files). Same generators as `dex bench eval`.

Curated and generated sets are kept as **separate report rows** — they measure
different things; never average across them.

## Usage

```sh
# score the whole corpus (fetches + indexes on first run; cached after)
dex bench corpus run

# fast local iteration: first curated set per repo, skip generated sets
dex bench corpus run --smoke --repos flask,gin

# regression gate: fail if ANY (repo, set) cell drops >0.02 vs the baseline
mooncake task corpus            # = DEX_DISABLE_RERANK=1 dex bench corpus run --k 10 --check ...

# refresh the committed baseline after an intentional improvement (same config
# as the gate: rerank-off; strip per-query detail to keep the file small)
DEX_DISABLE_RERANK=1 dex bench corpus run --output json \
  | jq 'del(.cells[].report.queries)' > benchmark/corpus/baseline.json
```

Requires a built `dex` on PATH, a live embed endpoint (`DEX_EMBED_URL` / ollama),
and network access to clone the repos on first run. Like `dex bench eval`, this
is a **local / main_pc gate, not a container-CI step**.

**Why rerank-off (`DEX_DISABLE_RERANK=1`):** the gate isolates the fusion layer.
It is deterministic and immune to the shared-GPU rerank-endpoint contention that
flakes long runs on main_pc (a transient `context canceled` mid-scoring). The
baseline is generated under the same flag so the comparison is apples-to-apples.
Re-enable rerank for ad-hoc full-path A/Bs; keep the committed baseline rerank-off.

## Adding a repo

1. Add an entry to `repos.yml` (pin to a release tag's **commit**, not the tag
   object — resolve the peeled commit with
   `git ls-remote <url> 'refs/tags/<tag>^{}' refs/tags/<tag>` and take the `^{}`
   SHA when present). `commit` must be the 40-hex commit `HEAD` resolves to.
2. Enable a `gen` flavor for instant auto-labels, and/or add a curated query set:
3. Write `queries/<name>.json` as an `eval.GoldenSet` — `~15–40` queries spanning
   concept / symbol-API / cross-file capability classes. **Read-verify every
   `relevant_files` path at the pinned commit.**
4. `dex bench corpus run --repos <name>` to check it, then refresh the baseline.

No Go change is needed to add a repo.

### Large monorepos — `index_subdir`

For a flagship monorepo, bound the index cost by pointing `index_subdir` at one
substantive package. The indexed root then becomes `<checkout>/<index_subdir>`,
so **curated `relevant_files` paths must be relative to that subdir** (e.g. with
`index_subdir: packages/react-dom-bindings`, label `src/events/SyntheticEvent.js`,
not the full `packages/...` path). Pick the package that actually holds the logic
you want to query — e.g. React's public `packages/react-dom` is a thin
entry-point shell; the real DOM/event implementation lives in
`packages/react-dom-bindings`, which is what the corpus indexes.

> **`gen` is incompatible with `index_subdir`.** The auto-label generators run
> `git log` in the subdir but git emits repo-root-relative paths, which then fail
> the gen "file exists on disk" check (resolved against the subdir) — so the cell
> comes back empty. Subdir-scoped repos must rely on a curated `query_sets`.

## Files

- `repos.yml` — pinned corpus manifest.
- `queries/<name>.json` — committed curated query sets (`eval.GoldenSet` shape).
- `baseline.json` — committed metric baseline the `--check` gate compares against.
