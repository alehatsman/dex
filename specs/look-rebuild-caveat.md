---
title: look — flag index-backed empties during a rebuild (#152)
status: draft
covers:
  - internal/mcp/look.go
  - internal/mcp/look_freshness.go
extends:
  - specs/envelope-uniform.md   # #110 EnvTrust — the one trust shape
  - specs/tool-surface.md       # #110 four-verb surface
---

# look — rebuild caveat for empty index-backed lanes (#152)

## Goal

`look` must never present an **index-backed empty** as authoritative absence
while the index is being destructively rebuilt. Today `lookVerb` stamps
`Trust: exactTrust()` (`provenance:"exact"`, `fresh` unset) on every lane, so a
`trace`/`locate` that returns `not-found` *because the graph was truncated
mid-`dex reindex`* is indistinguishable from a genuine "this symbol has no
callers." That false-absence is the trust hazard #152 reports.

## Root cause & scope

Two facts bound the fix:

1. The normal `dex index` / `watch` run is **upsert-under-new-timestamp then
   `PruneUnseen`** (store.go): old chunks/edges survive until the final prune, so
   a racing reader sees a *complete* prior generation — never an empty window.
   The empty window exists **only** during a destructive `dex reindex` (or the
   first-ever index of a project), when the graph is genuinely 0-row for a beat.
2. `grep` and `read` are **disk-authoritative** — `walkProjectFiles` reads the
   live working tree and the trigram cache is in-process; a reindex never touches
   source files. Their empties are ground truth and MUST NOT be caveated (a grep
   correctly matches even against a zero-chunk index). Only `trace` and `locate`
   read the SQLite graph index and can be transiently wrong.

So the caveat is scoped to `trace`/`locate` and gated on an *actual* in-progress
rebuild — not on age, not on emptiness alone.

## Design

- **`indexingNotice` already exists** (`index_signal.go`) and reads the
  cross-process `indexing_at` marker `dex reindex` sets (`SetIndexing` /
  `ClearIndexing`). `ask` (context.go) and `search` (server_search.go) already
  consult it; `look` does not. This fix reuses it — no new signal.
- **`indexProber` optional interface** (mirrors `seenLooker`): `*Server`
  implements `indexRebuilding(ctx, root) (bool, string)` by resolving the project,
  opening its cached store, and returning `indexingNotice`. `projectScoped`
  delegates. Any resolve/open error → `(false, "")`: an unavailable index is the
  lane's own `no-index` path, not a rebuild caveat.
- **`flagRebuildIfEmpty`** runs only for `trace`/`locate` and only when their
  status is an authoritative-looking empty (`not-found`, `no-graph`, `no-path`).
  When a rebuild is in progress it downgrades the envelope: `Trust.Fresh=false`,
  `Trust.Caveat` = the retry note, and appends the indexing hint. `provenance`
  stays `"exact"` (the lane *is* exact) — `fresh:false` is the honest signal that
  the index it read is being rewritten.
- The probe is paid **only on the empty index-backed path** (rare), and opens the
  already-cached store — no extra cost on the common hit path.

## Non-goals

- Retry-on-empty inside dex (the issue's option (a)): rejected — it masks genuine
  empties, and the honest caveat lets the *agent* decide to retry.
- Touching `grep`/`read` (disk-authoritative — see Root cause).
- The observed `grep` 0→6 in the #152 report: traced to disk/one-off, not an
  index race; grep needs no change. Documented on the issue.

## Validation

- **Unit:** `flagRebuildIfEmpty` with a fake `indexProber` → `not-found` +
  rebuilding stamps `fresh:false` + caveat; `not-found` + not-rebuilding leaves
  `exact` untouched; `ok` + rebuilding is untouched (only empties are caveated);
  a lane without the prober interface is a no-op.
- **Regression:** existing look tests (grep/read/error) keep `provenance:"exact"`
  and no caveat.
- `mooncake task ci` green.
