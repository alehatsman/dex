---
id: watch
status: living
owners: [aleh]
covers:
  - "internal/watch/**"
  - "internal/index/drain.go"
---
# Watch

## Intent

`dex watch` keeps a project's index hot without a human re-running the indexer.
It watches the opted-in tree for save events, coalesces a burst of edits into a
single re-index pass, and then — while the developer is idle — spends that quiet
time composing the LLM summaries that the search surfaces read. The goal is a
"set it and forget it" daemon: edits land, the index catches up within a
debounce window, and the more expensive summary roll-up (file → chunk →
package → repo) drains in the background and yields the moment editing resumes.
This spec covers the daemon's scheduling and lifecycle; producing the index and
the summaries themselves belongs to the indexing spec — watch only triggers it.

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
- WHEN a re-index pass leaves the index clean (no pending events), dex arms an
  idle timer (default 5 s); WHEN that idle window elapses undisturbed, the idle
  hook runs to drain pending summaries.
- WHILE the idle hook runs, it drains queued file/chunk summaries in bounded
  batches and, WHEN the pending-summary queue reaches empty, cascades the roll-up
  to package and repo summaries, so a watch-driven backfill converges to the same
  summary state a manual `dex index summarize` would produce.
- WHILE summaries drain, dex holds a per-project summary-drain lock distinct from
  the index lock, so only one process drains a project's queue at a time and a
  manual `dex index summarize` and the daemon never double-generate the same rows.
- WHEN a fresh filesystem event arrives during a pending or in-flight idle drain,
  dex preempts it — stopping the idle timer and cancelling the drain's context —
  so a long summary run yields immediately to the developer's edits; expensive
  chat/embed calls observe the cancellation and abort promptly.
- WHILE a package/repo cascade is preempted mid-flight, each package summary the
  chat backend already produced is committed durably so it survives the
  cancellation, letting the cascade converge across later flush→idle retries
  rather than discarding completed work on every interruption.
- WHILE a foreground query ran within the configured yield window, the idle
  drainer defers and re-arms rather than competing with interactive work for the
  embedding/chat backend.
- IF the idle drainer makes no progress across consecutive ticks (e.g. the chat
  backend is down), dex backs off exponentially and stops busy-looping until the
  next re-index re-arms the cycle.

## Non-goals

- **Producing the index and the summaries.** Walking, chunking, embedding, and the
  prompt shapes for file/chunk/package/repo summaries are the **indexing** spec;
  watch only schedules and triggers that pipeline.
- **What counts as watchable.** The ignore/allowlist rules that decide which paths
  emit relevant events are the **ignore** spec; watch consults them.
- **The on-disk queue and lock primitives.** The `pending_summaries` table and the
  flock semantics are owned by **storage**; watch only acquires the drain lock and
  asks the indexer to drain.
- **Serving queries.** `semantic-search`, `symbol-search`, `graph`, and `ask`
  read the index watch keeps hot; their behavior is specified separately.

## Checklist

- [x] Recursive fsnotify watch; ignored subtrees skipped; new dirs auto-watched
- [x] Initial re-index on startup
- [x] Debounce/coalesce burst of events into one pass (default 500 ms)
- [x] Single-flight re-index; mid-pass events re-run in the same goroutine
- [x] AfterIndex hook (graph refresh) on success; errors logged, loop continues
- [x] Idle timer after a clean flush; OnIdle drains pending summaries in batches
- [x] Cascade to package + repo summaries once the pending queue empties
- [x] Per-project summary-drain lock distinct from the index lock
- [x] Fresh events preempt pending/in-flight idle drain (timer stop + ctx cancel)
- [x] Foreground-yield + exponential backoff in the idle drainer
- [ ] Idle/watch-driven package+repo cascade survives preemption and converges
      across retries (durable per-package commit) — tracked by #33
- [x] Verified against the code by the verify workflow (flip to `living`)
