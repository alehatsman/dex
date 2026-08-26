---
title: Temporal coupling — git-history co-change edges (#212)
status: draft
covers:
  - internal/gitlog
  - internal/graph/graph.go
  - internal/graph/cochange.go
  - internal/store/store_graph.go
  - internal/eval/cochange/cochange.go
  - cmd/dex/bench_cochange.go
extends:
  - specs/graph.md
---

# Temporal coupling — git-history co-change edges (#212)

## Goal

`dex bench cochange` (#555) measures a persistent ceiling: blast-radius gold
pairs that co-change in commits but aren't call/import connected are
unreachable through the structural graph on every corpus repo (flask≈43%,
react≈21%, gin≈37%, ripgrep/zod≈0% two-hop). The coupling is real but
non-structural — it lives in commit history, not in the AST. Mine it directly
(à la Code Maat / CodeScene temporal-coupling analysis) and expose it as a new
graph edge kind, `co_changes`, kept fully separate from `calls`/`imports` so
existing structural consumers (PageRank, Louvain, `impact`, `trace`) are
unaffected. Measure the effect with a second, additive `dex bench cochange`
pass that includes `co_changes` in reachability.

## Scope

In scope:
- Mine `git log --name-only` for the current project during graph indexing;
  compute file-pair co-change support/confidence; emit `co_changes` edges
  between existing `file` nodes.
- New edge kind `EdgeCoChanges` in `internal/graph`.
- A new low-level shared package, `internal/gitlog`, housing the commit-log
  mining primitive so both `internal/graph` (indexing) and `internal/eval`
  (golden-set generation, bench) can use it without an import cycle.
- `dex bench cochange` gains a second, additive reachability metric
  (structural+temporal) alongside the existing structural-only one.
- Unit tests for the mining algorithm (pair-counting, support/confidence,
  maxFilesPerCommit guard, file-existence filter, dedup ordering).

Out of scope (v1):
- Symbol-level (function/hunk) coupling — file-level only, per the issue's
  approach sketch.
- Using `co_changes` in ranking/fusion/PageRank/Louvain — this issue only
  wires the edge kind and measures the ceiling; using it to re-rank retrieval
  is a follow-up.
- A `[graph]` config section. Gated by a single env var (see Config below).
- Non-Go/JS/etc. distinction — mining is language-agnostic (it only reads
  `git log`), so it runs once per project regardless of extractor languages.

## Why a new package (`internal/gitlog`)

`internal/eval` already imports `internal/graph` (golden-set generation reads
the graph view). `internal/graph` must not import `internal/eval` — that would
be a cycle. The git-log mining logic (`collectCommits` in
`internal/eval/golden.go:155`) is a clean, dependency-free primitive (just
`os/exec` + `internal/gitenv`), so lift it into a new leaf package,
`internal/gitlog`, that both `internal/graph` and `internal/eval` import.
`internal/eval/golden.go` is refactored to call the shared primitive instead
of keeping its own copy — behavior-preserving, same output shape.

```
internal/gitlog        (new, leaf: os/exec + internal/gitenv only)
   ├── internal/graph      (co-change mining during indexing)
   └── internal/eval       (golden-set generation, existing collectCommits caller)
```

### `internal/gitlog` API

```go
package gitlog

// Commit is one non-merge commit's short hash, subject, and changed-file list.
type Commit struct {
    ShortHash string
    Subject   string
    Files     []string
}

// Collect runs `git log --no-merges --name-only --relative` against root and
// parses up to max commits. Same parsing contract as the historical
// eval.collectCommits: NUL-delimited `<hash>\x00<subject>` metadata lines,
// changed files one per line until the next metadata line. Returns (nil, nil)
// on a repo with no commits (git exits nonzero with empty output) — not an
// error, callers treat it as "no history to mine".
func Collect(ctx context.Context, root string, max int) ([]Commit, error)
```

`internal/eval/golden.go`'s `commitRec` becomes a thin alias/wrapper (or is
replaced outright) around `gitlog.Commit` — same three fields, so the
call-site diff in `golden.go` is mechanical.

## Data model

No migration: `graph_edges` (kind/src_id/dst_id/file_path/start_line/end_line/
metadata_json) is already kind-agnostic (`internal/store/store_migrate.go`).

`internal/graph/graph.go`, alongside the existing `EdgeKind` consts:

```go
// EdgeCoChanges is a non-structural edge mined from git history: two files
// that change together across commits above a support/confidence threshold,
// independent of any call/import relationship. Src/dst are file nodes only
// (never function/type/package nodes), deduped so SrcID < DstID
// lexicographically — the relationship is symmetric, one edge per pair.
// Metadata carries {"support": <int>, "confidence": <float64>}.
EdgeCoChanges EdgeKind = "co_changes"
```

Edge shape emitted by mining, for each qualifying file pair (A, B) with
A < B lexicographically by file node ID:
- `Kind`: `EdgeCoChanges`
- `SrcID` / `DstID`: the two files' `NodeFile` node IDs (from the just-extracted
  node set — see Wiring)
- `FilePath`: `""` (a file-pair edge has no single "location"; distinct from
  calls/imports which are anchored at a call site)
- `StartLine` / `EndLine`: `0`
- `Metadata`: `{"support": support, "confidence": confidence}`

`EdgeID` (existing helper, `srcID+kind+dstID+filePath+startLine`) already
produces a stable PK for this shape (filePath="" and startLine=0 are constant
per pair, so re-running mining on an unchanged history upserts in place, not
duplicates).

## Algorithm

Given a project root:

1. `gitlog.Collect(ctx, root, MaxCommits)` — `MaxCommits = 2000` (default,
   const in `internal/graph`; git-log walk cap, same cap style as other
   history-mining call sites in this codebase).
2. For each commit:
   - If `len(commit.Files) > maxFilesPerCommit` (`30`), skip it entirely — a
     mass-rename/vendor-bump/formatting commit is noise, not coupling signal.
   - Else, for every unordered pair `{A, B}` of distinct files in
     `commit.Files`, increment `pairCount[{A,B}]`. Also increment
     `commitsTouching[A]` and `commitsTouching[B]` once each (not once per
     pair) for every file in the (non-skipped) commit.
3. For each pair with `pairCount >= support (2)`:
   - `confidence = pairCount / min(commitsTouching[A], commitsTouching[B])`
   - Drop if `confidence < 0.1`.
4. Filter to pairs where **both** A and B resolve to an existing `file` node
   in the current run's freshly-extracted node set (`result.Nodes`, kind
   `NodeFile`, matched by `FilePath`). A path that git tracks but the graph
   never emitted a file node for (deleted, gitignored, unsupported extension,
   binary) is dropped — the edge would dangle.
5. Dedupe direction: canonicalize each surviving pair so `SrcID < DstID`
   lexicographically (file node IDs, not paths — paths could collide in
   theory across packages for non-Go languages; node IDs are the real PK
   space). One edge per pair regardless of iteration order.
6. Emit the edge list; append to `result.Edges` before the final
   `GraphUpsertEdges` call in `Indexer.Run`.

Complexity note: step 2's per-commit pairing is O(k²) in files-per-commit;
the `maxFilesPerCommit=30` skip bounds the worst case per commit to 30²=900
pair increments, and `MaxCommits=2000` bounds total work — acceptable for a
one-shot mining pass during indexing (not per-query).

## Wiring

New file `internal/graph/cochange.go`:

```go
// mineCoChanges mines git history for file-level temporal coupling and
// returns co_changes edges between file nodes present in nodes. Best-effort:
// a non-git root or a history-read error yields (nil, nil) — indexing must
// not fail because git-log mining failed (mirrors ExtractGo/ExtractYAML's
// tolerance of a partial/absent input; unlike those, an all-or-nothing
// history read has no partial state to lose).
func mineCoChanges(ctx context.Context, root string, nodes []Node) ([]Edge, error)
```

Called from `Indexer.Run` in `internal/graph/graph.go`, after the sitter
extraction block and before the empty-extraction guard, using the merged
`result.Nodes` (so file nodes from every extractor — Go/YAML/Markdown/sitter —
are eligible endpoints):

```go
if coChangeEnabled() {
    ccEdges, err := mineCoChanges(ctx, g.project.Root, result.Nodes)
    if err != nil {
        g.log.Warn("co-change mining failed, skipping", "error", err)
    } else {
        result.Edges = append(result.Edges, ccEdges...)
    }
}
```

Placed after node merging (so all `NodeFile` nodes across languages are
available for the existence filter) but before the empty-extraction guard —
if extraction otherwise produced 0 nodes/edges, co-change edges alone must
not defeat that guard (co-change-only graphs are not a supported state); gate
the mining call itself on `len(result.Nodes) > 0`.

## Config

No `[graph]` config section for v1 (none exists today; adding one is out of
scope). A single env var toggle, mirroring `DEX_FEEDBACK_LIVE`
(`internal/mcp/feedback_shadow.go`):

```go
// DEX_GRAPH_COCHANGE=0 disables git-history co-change mining during graph
// indexing (default: on). Off switch for repos where git-log mining is
// unwanted/slow (huge history, shallow clone with no log, non-git root).
func coChangeEnabled() bool { return os.Getenv("DEX_GRAPH_COCHANGE") != "0" }
```

Default-on: the mining pass is bounded (`MaxCommits=2000`, `O(commits ×
maxFilesPerCommit²)`) and best-effort-tolerant of failure, so it's cheap
enough to run unconditionally, consistent with the issue's "if cheap,
default-on" framing.

## Bench: `dex bench cochange` — additive second metric

`internal/eval/cochange/cochange.go`:
- Keep `connectingKinds = []graph.EdgeKind{EdgeCalls, EdgeImports}` and
  `Report`/`Compute` **exactly as-is** — this is the committed baseline
  (`benchmark/.../cochange-baseline.json` or similar, checked via `--check`)
  and the issue's success criteria requires it to show zero regression.
- Add a second computation path that includes `EdgeCoChanges` in the
  adjacency (`connectingKinds` ∪ `{EdgeCoChanges}`), producing a **separate**
  `Report`-shaped result (reuse `Report`'s shape, new field or new struct —
  e.g. `Cell.WithCoChange *Report`) so the two never merge into one number.
- `buildFileAdjacency` gains a `kinds []graph.EdgeKind` parameter (or a
  sibling function) so both passes share the BFS/reach logic.

`cmd/dex/bench_cochange.go`:
- Compute both reports per repo/project (structural-only baseline,
  structural+temporal).
- Markdown/JSON output gains two more columns/fields: `1-hop%(+cc)` /
  `2-hop%(+cc)` (or a `_cc` suffixed JSON field) alongside the existing
  `1-hop%` / `2-hop%`.
- `--check` drift gating stays anchored to the **existing** (structural-only)
  `TwoHopShare` — the regression-safety contract in the issue — the new
  +cc numbers are reported, not gated, in v1.

## Validation

Unit tests (`internal/graph/cochange_test.go`, `internal/gitlog/gitlog_test.go`):
1. Pair-counting/support/confidence math on a synthetic commit log (hand-built
   `[]gitlog.Commit`, no real git repo) — verify support and confidence
   arithmetic against hand-computed expected values, including the
   `commitsTouching` per-file (not per-pair) increment.
2. `maxFilesPerCommit` guard: a commit touching >30 files contributes zero
   pairs and zero `commitsTouching` increments.
3. File-existence filter: a pair where one file has no corresponding
   `NodeFile` node is dropped.
4. Dedup ordering: feeding the same pair in both orderings across two
   (synthetic) inputs yields exactly one edge, `SrcID < DstID`.
5. `gitlog.Collect` parsing: reuse/adapt the existing `collectCommits`
   parsing test coverage (NUL-delimited metadata line, multi-file blocks).

Integration/manual: after `mooncake task install`, reindex this repo
(`dex index --graph=only` or a full index) and confirm `co_changes` rows
appear in `graph_edges` (e.g. via `graph_edges WHERE kind='co_changes'` or a
`dex` query lane that surfaces edge kinds). Run `dex bench cochange run
--project . --lang go` and confirm the +cc columns are ≥ the structural-only
columns (co-change edges are strictly additive to reachability, never
subtractive).

## Success criteria (from the issue)

- Measurable increase in 1/2-hop reachability when `co_changes` is included,
  on repos where the corpus manifest is available (flask/react/gin/
  ripgrep/zod) or at minimum demonstrated on this repo via `--project .`.
- Zero change to the existing structural-only `Report`/`TwoHopShare` numbers
  (`connectingKinds` untouched, new metric fully additive) — enforced by
  `mooncake task ci-fast` plus the existing `--check` baseline continuing to
  pass unmodified.
