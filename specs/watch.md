---
id: watch
status: living
last_verified: 621894f
owners: [aleh]
covers:
  - "internal/watch/**"
---
# Watch

## Intent

`dex watch` keeps a project's index hot without a human re-running the indexer.
It watches the opted-in tree for save events and coalesces a burst of edits into
a single re-index pass. The goal is a "set it and forget it" daemon: edits land
and the index catches up within a debounce window. This spec covers the daemon's
scheduling and lifecycle; producing the index itself belongs to the indexing
spec — watch only triggers it.

## Behavior

- WHILE watching, dex registers a recursive filesystem watch over the project
  root, skipping any subtree the ignore rules exclude, and adds a watch to each
  new non-ignored directory as it is created (the underlying watcher is
  non-recursive).
- WHEN the daemon starts, it performs one initial re-index pass so changes made
  while it was stopped are picked up.
- WHEN a filesystem event arrives on a non-ignored, indexable path
  (create/write/remove/rename), dex marks the index dirty and (re)arms a debounce
  timer; the re-index runs only after a quiet window (default 500 ms) with no
  further events, so a burst of saves coalesces into one pass.
- WHEN the debounce window expires, dex runs a full indexer pass; because the
  indexer is content-hash incremental, only changed files are re-chunked and
  re-embedded, so a per-save walk is cheap.
- WHILE a re-index pass is running, dex does not start a second concurrent pass;
  events that land mid-pass leave the index dirty and the same goroutine re-runs
  the pass when it finishes, serializing work so two indexers never race the
  store's writer.
- WHEN a re-index pass succeeds and an after-index hook is configured, dex invokes
  it (used to refresh the Go static graph in lockstep with the chunk index); a
  hook error is logged but does not stop the watch loop.

## Non-goals

- **Producing the index.** Walking, chunking, and embedding are the **indexing**
  spec; watch only schedules and triggers that pipeline.
- **What counts as watchable.** The ignore/allowlist rules that decide which paths
  emit relevant events are the **ignore** spec; watch consults them.
- **Serving queries.** `semantic-search`, `symbol-search`, `graph`, and `ask`
  read the index watch keeps hot; their behavior is specified separately.

## Checklist

- [x] Recursive fsnotify watch; ignored subtrees skipped; new dirs auto-watched
- [x] Initial re-index on startup
- [x] Debounce/coalesce burst of events into one pass (default 500 ms)
- [x] Single-flight re-index; mid-pass events re-run in the same goroutine
- [x] AfterIndex hook (graph refresh) on success; errors logged, loop continues
- [x] Verified against the code by the verify workflow (flip to `living`)
