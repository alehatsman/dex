# Design — #95e Hierarchical (package/subsystem) summaries

Status: **design / spec** · Child of [#95](95-architecture.md) · design origin
§5 step 4, WS5. Validate against the code, then build.

## TL;DR

`file_summaries` gives dex one LLM prose summary per file, gated by a content
hash so `dex summarize` only regenerates what changed. #95e extends that
*upward*: a **rollup summary per directory** (package → subsystem → repo), each
built by summarizing its children's summaries, each gated by a **composite hash
of its children's hashes**. Touch one file and only its ancestor directories
re-roll — the same source-linked invalidation, one level up. Rollups are an
**isolated, opt-in derived artifact** (like `file_summaries` today): nothing in
the retrieval/fusion path reads them, so the default `ask` path is untouched.

## 1. What exists today (the pattern we extend)

- **Table** `file_summaries(path PK, source_hash, prompt_version, model,
  summary, generated_at)` — `store_migrate.go:304`. Deliberately isolated: no
  FTS trigger, no vector, no search path reads it.
- **Staleness signal** `source_hash = FileBodyHash(bodies)` — sha256 over the
  *sorted* set of a file's chunk `content_sha1`s (`store_summaries.go:60`).
  Body-change sensitive, line-shift invariant.
- **Gate** `FileSummaryMeta` returns stored `(source_hash, prompt_version)`;
  `dex summarize` regenerates a file only when `pv != summaryPromptVersion ||
  h != srcHash` (`main_summarize.go:121`).
- **Generation** `dex summarize` reads chunk bodies from sqlite, calls the chat
  backend with `summarize.BuildSystem`, upserts (`main_summarize.go:102`).

**Confirmed greenfield:** the `package_summary` / `repo_summary` chunk kinds
referenced in `results.go:31` and `context_test.go` are **never produced** —
they are a defensive filter + test fixtures only. No rollup summaries exist in
the tree today.

## 2. The rollup model — the directory tree

Every directory that (transitively) contains an indexed code file is a rollup
node. The node key is the **index-relative directory path**, forward-slashed;
the repo root is `""`. "Package" and "subsystem" are just heights in this tree —
we do not introduce a separate subsystem taxonomy (that would need a config the
repo doesn't have; the directory tree is the boring, already-present grouping).

A directory node's **children** are:
- the **files** directly in it that have a `file_summaries` row, and
- the **immediate subdirectories** that are themselves rollup nodes.

Directory membership is derived from `Store.CodeFilePaths` (`store_search.go:1206`)
— no new enumeration primitive.

## 3. Storage — a sibling table, not an overloaded one

New table `dir_summaries`, identical shape to `file_summaries`, added to
`schemaDDL()` and gated by bumping `schemaVersion` `"6" → "7"` (reindex
rebuilds — the existing fail-closed contract; no ALTER path).

```sql
CREATE TABLE IF NOT EXISTS dir_summaries (
  path           TEXT PRIMARY KEY,   -- index-relative dir; "" = repo root
  source_hash    TEXT NOT NULL,      -- composite hash over child source_hashes
  prompt_version INTEGER NOT NULL,
  model          TEXT NOT NULL,
  summary        TEXT NOT NULL,
  generated_at   INTEGER NOT NULL
)
```

Why a new table, not a `level` column on `file_summaries`:
- `file_summaries.source_hash` means *file body hash*; a dir's means *composite
  of child hashes* — different semantics under one column invites bugs.
- Two consumers read `file_summaries` today (`dex summarize --get`, mcp
  summarize verbs). A new table keeps them from suddenly seeing directory rows.
- Zero migration risk to existing file rows; both drop+rebuild on reindex.

## 4. Invalidation — composite hash (the core property)

```
dir.source_hash = RollupHash(childHashes)
  childHashes = { file_summaries.source_hash  for each child file  }
              ∪ { dir_summaries.source_hash   for each child dir   }
```

`RollupHash` reuses the exact `FileBodyHash` construction: sort the child
hashes, join with `\n`, sha256. Consequences:

- **Locality:** change one file → its `file_summaries.source_hash` flips → the
  parent dir's composite flips → grandparent flips → … → root. Every ancestor
  re-rolls; **no non-ancestor does** (untouched siblings keep their hash, so the
  parent's sorted child-hash set changes only in the one slot). This is the
  acceptance criterion "touching one file re-rolls only its ancestors", proven
  by construction.
- **Determinism:** the hash depends only on child *content* hashes, not on
  generated prose or timestamps, so a regenerate that produces identical inputs
  is a no-op on the gate.

`rollupPromptVersion` (a distinct const) forces a full re-roll when the rollup
prompt changes, mirroring `summaryPromptVersion`.

## 5. Generation — `dex summarize --rollups`

Opt-in flag on the existing command. Sequence:

1. Run the normal file pass (unchanged) so every child file summary is current.
2. Enumerate directories from `CodeFilePaths`; sort **deepest-first** (bottom-up)
   so a node's children are already current when we reach it.
3. For each dir: collect child hashes → `RollupHash` → `DirSummaryMeta` gate.
   If stale, build input from child **summaries** (each child file summary and
   child dir summary, prefixed with its name — we summarize summaries, never
   re-read raw code), call chat with `summarize.BuildRollupSystem`, upsert.

Skipped-when-fresh by the same hash gate, so re-running is cheap. Nothing reads
`dir_summaries` on the `ask` path, so **default latency is unchanged by
construction** (criterion 3).

## 6. Interfaces (new, all in `internal/store` + `cmd/dex` + `internal/summarize`)

```go
// store — mirrors the file_summaries trio.
type DirSummary struct { Path, SourceHash string; PromptVersion int; Model, Summary string; GeneratedAt time.Time }
func (s *Store) DirSummaryMeta(ctx, relDir) (sourceHash string, promptVersion int, ok bool, err error)
func (s *Store) GetDirSummary(ctx, relDir) (DirSummary, bool, error)
func (s *Store) UpsertDirSummary(ctx, DirSummary) error
func RollupHash(childHashes []string) string   // == FileBodyHash construction, exported for reuse

// summarize — the rollup prompt.
func BuildRollupSystem(focus string) string

// cmd/dex — the second pass.
func runRollupGenerate(ctx, st, root, focus string, force, verbose bool, format string) error
```

`RollupHash` and `FileBodyHash` share one helper (`hashSorted`) so the two can
never drift.

## 7. Scope — what #95e is NOT

- **No retrieval wiring.** Surfacing rollups as `package_summary` /
  `repo_summary` semantic hits (the filter in `results.go` already anticipates
  them) is a *separate* follow-up. #95e only *produces and invalidates* rollups.
- **No `watch` integration.** Rollups are CLI-triggered and cacheable this
  issue; incremental refresh on file-change events is later work.
- **No subsystem taxonomy.** Subsystem = an ancestor directory; we do not add a
  layer/grouping config.

## 8. Acceptance

- Package + subsystem (= directory) rollups generated incrementally,
  source-grounded (built from child summaries, hash-gated).
- **Invalidation test:** seed a small tree, roll up, mutate one file's chunk
  hash, re-roll; assert exactly the mutated file's ancestor chain regenerates
  and every other node is untouched.
- `dir_summaries` stays isolated from retrieval; the `ask` path reads nothing new.
- `mooncake task ci-fast` green.
