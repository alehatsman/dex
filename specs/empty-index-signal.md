---
id: empty-index-signal
status: accepted
last_verified: ceb7e33
owners: [aleh]
covers:
  - "cmd/dex/main_index.go"
  - "cmd/dex/main_manage.go"
  - "internal/store/store.go"
  - "internal/mcp/context.go"
tracking: alehatsman/dex#161
---

# Empty index — an honest signal, not a silent ✓

## Goal

An index with **0 chunks** must be distinguishable from a valid-but-unmatched
query, at both surfaces where the confusion bites:

1. `dex index` / `dex reindex` on a repo with no `index.include` must NOT print
   `✓ indexed` and exit 0. It walks the tree, skips every file (the include
   allow-list is opt-in and mandatory), and today dresses that up as success.
2. MCP `ask` against a 0-chunk index returns `status:ok` + `hint:"no matches"` +
   `fresh:true` — identical to a real miss. An agent then burns turns rephrasing
   a query against a repo that can never match. It must instead get a
   config-shaped signal.

## Root cause (recap of #161)

- The include allow-list is opt-in: `ignore.Matcher.Match` skips every file when
  `include == nil`. `Matcher.IncludeConfigured()` already reports this, and
  `dex doctor` already flags it — the detection exists, it just isn't reflected
  in the `index` exit signal or the MCP surface.

## Design

### Surface 1 — `dex index` / `dex reindex`

At the success-report site (`main_index.go` ~L165, `main_manage.go` ~L300), when
`stats.Chunks == 0 && !ig.IncludeConfigured()`:
- text mode: no `✓`; return a descriptive error → `main` prints `error: …` and
  exits 1.
- json mode: still emit the result object (machine consumers get structured
  `chunks:0`), then return the same error for the non-zero exit code.

Message: `nothing indexed: no index.include in <root>/.dex/config.yml — add an
include allow-list (run `dex doctor` for the diagnosis), then re-run dex index`.

Only the `!IncludeConfigured()` case is treated as an error. `0 chunks` WITH an
include configured is a different situation (over-narrow include, genuinely
empty tree) and keeps today's behavior — out of scope here.

### Surface 2 — MCP `ask`

New `store.Store.IsEmpty(ctx) (bool, error)` — a fast `SELECT EXISTS(SELECT 1
FROM chunks)` probe. In `context.go`, only when both lanes whiffed (keeps the
probe off the hot path), pass `indexEmpty` into `noLaneHits`. When the index is
empty, `noLaneHits` sets a new status BEFORE the embed-failed / no-match
branches (an empty index is the dominant, retry-proof cause):

- `status: "index-empty"`
- `hint`: points at `dex doctor` + `dex index`, names the likely `index.include`
  gap.
- `next_action`: tells the agent no query can match until the index config is
  fixed — do not rephrase.

`look` grep is unaffected: it falls back to a working-tree filesystem walk
(#132), so it is disk-authoritative even with a 0-chunk index.

## Edge cases

- DB file missing entirely → existing `no-index` status (unchanged); `index-empty`
  is specifically DB-present-but-0-chunks.
- `IsEmpty` query error → treated as non-empty (fail open to today's no-match
  path); never masks results.
- bm25-only / lean profiles: emptiness is engine-independent (chunk rows), so the
  signal is correct there too.

## Out of scope (deferred)

- `dex init` scaffold to seed a starting `.dex/config.yml` (#161 fix 2) — a new
  command / product decision.
- The Go-centric "graph extraction produced 0 nodes … missing go.mod" message on
  non-Go repos (cosmetic).

## Validation

- Unit: `IsEmpty` true on fresh store, false after a chunk upsert.
- Unit: `noLaneHits` with `indexEmpty=true` yields `index-empty` status ahead of
  embed-failed/no-match.
- `mooncake task ci` green.
- Live: `dex index` a no-`index.include` repo → non-zero exit + distinct message;
  MCP `ask` there → `status:index-empty`.
