---
id: review
status: living
last_verified: c4b4bdc
owners: [aleh]
covers:
  - "internal/review/**"
  - "cmd/dex/review.go"
  - "internal/mcp/server_review.go"
---
# Review

## Intent

Collapse per-hunk PR intelligence into a single call. Code review is
delta-shaped, but every other dex verb is state-shaped — ask/find/trace/read
give a current snapshot. To review a diff an agent otherwise stitches together
six tools per file: diff, callers per touched symbol, sibling tests, git history,
linked notes. `review` replaces that chain with one structured result. No chat
model is required; all lanes are structural (graph, git metadata, store notes).

## Behavior

**Input selection** (`ReviewInput`):

- Exactly one of `ref`, `branch`, or `pr` is expected; precedence is
  `ref` > `branch` > `pr`.
- IF none is provided, the verb returns `status=error`.
- WHEN `ref` is a single ref without `..` (e.g. `HEAD~3`), it is expanded to
  `ref..HEAD`.
- WHEN `branch` is given, the range is `base...branch` (three-dot symmetric
  diff — what the branch adds since diverging from `base`; `base` defaults to
  `main`).
- WHEN `pr` is given, resolves the head branch via `gh pr view <n> --json
  headRefName`, then treats it as `branch`. Best-effort: requires the `gh` CLI,
  a GitHub remote, and a fetched head; returns `status=not-found` on failure.
- CLI default (no selector): `HEAD~1..HEAD`.

**Diff acquisition**: runs `git diff --unified=0 --no-color <range>` in the
project root (5-second timeout, 4 MB hard cap on raw bytes). Raw input uses zero
context lines so hunks stay tight around the actual change.

**Diff parsing** (`review.ParseUnified` in `internal/review/diff.go`):

- Self-contained: no git invocation, no store, no model. Unit-testable against
  literal diff text.
- Tolerates all `git diff` header lines (`index`, `similarity index`, mode
  changes, binary markers) — skips them silently.
- A malformed hunk header is skipped rather than fatal, so one odd file never
  sinks the whole review.
- `FileDiff.Status`: `added | modified | deleted | renamed`.
- `FileDiff.OldPath` is non-empty only on renames (distinct from `Path`).
- `Hunk.TouchedLines()` returns the new-side line numbers the hunk introduces or
  modifies; a pure deletion (NewLines=0) yields the anchor line so the symbol
  lane still has something to probe.

**Hunk→symbol mapping**:

- Uses `store.ChunkAt(ctx, path, line)` to resolve a new-side line to its
  enclosing declaration (decl byte-spans from #591).
- Probes at most `reviewMaxProbes=32` lines per hunk, strided evenly so a large
  added file costs a bounded number of lookups.
- Caps symbols per hunk at `reviewMaxSymHunk=6`.
- Deduplicates by symbol name: a long function spans multiple indexed chunks with
  different start lines, but they're the same symbol. Method names are
  receiver-qualified in the index so different-receiver methods with the same
  bare name don't collide.
- A deleted file (`Status=deleted`) skips symbol resolution — no current symbols
  to map.

**Callers lane**:

- For each resolved symbol calls `traceVerb(direction=callers, k=k)`.
- Memoised per symbol name across the whole review (callerCache) — a function
  touched in multiple hunks/files is only looked up once.
- IF the store has no graph (`status=no-graph`), the callers list is empty and
  `hadGraph=false` is propagated to `hunkRisk`.
- `k` defaults to 30, capped at 30. (The default 30 matches `reviewCallerHigh`
  so the high-risk threshold is reachable.)
- **Callers are hoisted to a single top-level `callers_by_symbol` map** keyed by
  symbol name (#136). A symbol touched by N hunks previously duplicated its full
  caller list — each with source content — N times, the largest payload in the
  response. Now each hunk names its symbols in `symbols_touched` (each carrying a
  `caller_count`); the caller bodies live once in `callers_by_symbol[name]`. Only
  symbols present in emitted hunks (post-`compact`, post-truncation) are included.

**Risk tier** (`hunkRisk`):

- Thresholds on the max caller count across symbols in the hunk:
  - `>=30 callers` → `high`
  - `>=10 callers` → `medium`
  - otherwise → `low`
- An exported symbol bumps the tier one step (low→medium, medium→high).
- WHEN the graph is not indexed, tier falls back to export status only; reason
  text says "callers unknown (graph not indexed)".
- `compact=true` drops all low-risk hunks; files left with zero hunks are
  omitted entirely.

**File-level lanes** (run once per file, best-effort):

- `tests_covering`: sibling test files via `Enricher.pairSiblingTests`.
- `nearest_doc`: nearest doc file via `Enricher.findNearestDoc`.
- `last_commit`, `last_author`: git blame via `Enricher.enrichBlame`.
- `churn_30d`: `git rev-list --count --since="30 days ago" HEAD -- <path>`.
- `author_history`: last 3 commit authors via `git log -3 --format=%an -- <path>`.
- `scoped_notes`: notes whose `scope` binds this file's path
  (`store.KnowledgeByScope`), surfaced because the PR touches the file
  (gotcha-on-touch #645/#649). Distinct from per-hunk notes recalled by symbol.
  This is the read side of the review→edit loop (#87): a prior review's
  `ReviewFinding` notes (see specs/review-finding.md) land here on the very files
  a later PR touches.

When a review returns files, its `hint` nudges the reviewer to persist a
confirmed finding as `notes(action=add, archetype=ReviewFinding, scope=<file>,
body="[kind] …")` — closing the loop where the review happens (#87).

**Hunk-level notes**:

- Per symbol: `recallFacts(symbol, k)` — memoised per symbol name (noteCache).
- Deduped by note ID within a hunk so two symbols sharing a note don't produce
  duplicates.

**Caps and truncation**:

- Files: `reviewMaxFiles=100`. Files beyond the cap are dropped and
  `Truncated=true`.
- Total hunks: `reviewMaxHunks=200`. Processing stops when the budget is
  exhausted; `Truncated=true`.
- Non-code file guard: WHEN a file has no indexed symbols after
  `reviewMaxHunksNoCode=3` hunks, it is treated as data/generated and capped
  at those 3 hunks, preserving the hunk budget for code files.

**Index requirement**: WHEN the project's `.dex/index.db` does not exist, returns
`status=no-index` with a hint to run `dex index <path>` first.

**Range-vs-index skew**: WHEN the range does not end at HEAD (e.g. reviewing an
older commit range), a hint is appended to the output explaining that
symbol/caller mapping is against the current index and may be incomplete.

**Output** (`ReviewOutput`):

```
status      string       // ok | no-index | no-changes | not-found | error
hint        string
project     string
range       string       // resolved git range actually diffed
total_hunks int
truncated   bool
callers_by_symbol map[string][]CallSite  // #136: caller bodies once per symbol, not per hunk
files       []ReviewFile
  file             string
  old_path         string        // non-empty on renames
  status           string        // added | modified | deleted | renamed
  tests_covering   []string
  nearest_doc      string
  churn_30d        int
  last_commit      string
  last_author      string
  author_history   []string
  scoped_notes     []LocatedFact
  hunks            []ReviewHunk
    old_start, old_lines, new_start, new_lines  int
    heading          string
    symbols_touched  []ReviewSymbol{name,kind,exported,start_line,end_line,caller_count}
    notes            []LocatedFact
    risk_tier        string   // low | medium | high
    risk_reason      string
```

Look up a touched symbol's callers via `callers_by_symbol[symbols_touched[i].name]`;
the per-symbol `caller_count` tells you how hot it is without the join.

**CLI** (`dex review`):

- Flags: `--ref`, `--branch`, `--pr`, `--base` (default `main`), `--compact`,
  `--k`, `--format text|json`.
- Takes an optional `<path>` positional arg (project root); no other positional args.
- Text output renders one section per file with per-hunk risk tiers, symbol list
  (each with its caller count), and note count. Non-ok status prints to stderr.

## Non-goals

- **Does not write or modify files.** Read-only throughout.
- **No chat model.** All lanes are structural; the verb never calls an LLM.
- **Hunk→symbol is enclosing-declaration, not call-site precise.** The graph
  stores call edges with start_line only, no reference byte spans, so a hunk is
  attributed to the function it lives in (#639 scope note).
- **Non-git repos are not supported.** The verb requires a git working tree; there
  is no fallback for non-git directories.
- **PR resolution requires local gh CLI and a fetched head.** Remote-only refs
  without a local fetch will fail at diff time even if the branch resolves.
- **No cross-repo reviews.** `projectRoot` scopes both the git diff and the index
  lookup; remote refs from other repos are outside scope.

## Checklist

- [x] All three selectors (ref/branch/pr) resolve to a valid git range
- [x] Single-ref input auto-expanded to `ref..HEAD`
- [x] Branch uses three-dot symmetric range (`base...branch`)
- [x] PR resolution is best-effort and returns `not-found` gracefully
- [x] Diff parser is self-contained (no git invocation, no store, no model)
- [x] Malformed hunk header skipped rather than fatal
- [x] Hunk symbol probing strided and capped (`reviewMaxProbes=32`, `reviewMaxSymHunk=6`)
- [x] Callers and notes memoised per symbol across the whole review
- [x] Deleted files skip symbol resolution
- [x] Non-code file guard caps data files at 3 hunks
- [x] Scoped notes (gotcha-on-touch) surfaced at file level
- [x] Risk tier degrades gracefully when graph is absent
- [x] `compact` mode drops low-risk hunks and empty files
- [x] `no-index` status returned when db is missing
- [x] Range-vs-index skew hint emitted when range doesn't end at HEAD
- [x] Verified against the code by the verify workflow (flip to `living`)
