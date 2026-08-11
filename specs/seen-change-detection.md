# Spec: seen-ledger change detection (#138)

Status: proposed
Epic: #110 tool-surface cutover, step 3 (session spine)
Issue: #138

## Goal

The cross-turn seen-dedup ledger must never suppress bytes that **changed** since
the agent last saw them. Today it does: a range surfaced on turn 1 and edited by
turn 3 is still blanked and stamped `seen turn 1`, so the agent never sees the new
content. This is the session-spine spec's own unmet bullet — "a seen file that
changed returns as a delta, not a re-dump" (`specs/tool-surface.md`).

## Scope

- In: `internal/mcp/seen.go` (`applySeenContext`) + `seen_test.go`.
- Out: line-level "delta" rendering (a follow-on); making dedup mandatory on
  `look`/`read`/`act` responses (the rest of step 3 — separate slice); any change
  to the wire schema or the `retrieve`/`store` layers.

## Background — what the ledger is

`applySeenContext(sessionKey, out)` runs once per `ask`, before budget capping
(`context.go:580`, ahead of the `:626` cap). It walks the three inlined lanes —
`SemanticHits[].Content`, `Symbols[].Body`, `SuggestedReads[].Content` — and for
any locator already surfaced on an **earlier** turn this session, blanks the heavy
field and stamps `SeenTurn = firstTurn`. State is per-session `seenState{turn int,
first map[string]int}` guarded by `seenMu`; the key is `locatorKey(path,start,end)`.

Two facts from the code that drive the design:

1. **The overlay content is live disk bytes.** All three lanes get their content
   from `inliner.fetch` (`internal/retrieve/inline.go:170`), which calls
   `source.ReadLineRange(abs, …)` against the working tree at request time (not a
   store snapshot), cached per `(path,start,end,caps)`. So for a given range the
   bytes are identical across lanes, and they genuinely change turn-to-turn the
   moment the agent edits the file — no reindex required. This is the case #138
   targets.

2. **Except summary-kind reads.** A `SuggestedRead` (or `SemHit`) that arrives with
   `Content` already set (summary kinds) skips `fetch` (`inline.go:220,332`). So a
   summary read and a raw semantic hit at the *same* locator can legitimately carry
   *different* bytes.

## The failed naive fix (why the obvious version is wrong)

First attempt: store one fingerprint per locator (`first map[string]seenRec{turn,
fp}`); on a repeat, suppress only if `fp` matches, else re-inline. The existing
`TestSeenMarksRepeatsAcrossTurns` caught this. The ledger key is **shared across
lanes** by design (a range seen via any lane dedups via any other). One `fp` per
shared key cannot represent two lanes that carry different bytes for the same
locator (raw vs summary, per fact 2): whichever lane writes the entry wins, and the
other lane's later repeats mis-compare. Storing a single canonical fingerprint —
e.g. threading `store.ContentSHA` up through `retrieve` into an unexported struct
field — was the first-considered remedy, but it is heavier (cross-layer plumbing +
a per-lane opt-out for rows lacking a SHA) and it *still* forces one fingerprint
onto two genuinely-different renderings. Rejected.

## Design — fold the fingerprint into the key

Key the ledger on **content**, not just position:

```
key = locatorKey(path,start,end) + ":" + fingerprint(bytes)
```

`fingerprint` is fnv64a over the lane's inlined bytes (`Content`/`Body`). Keep
`first map[string]int` (locator+fp → first-seen turn). `note` suppresses a lane's
field only when *that lane's exact bytes* were surfaced on an earlier turn.

Why this is correct where the naive version wasn't:

- **Change detection (the bug):** file edited between turns ⇒ new disk bytes ⇒ new
  fingerprint ⇒ new key ⇒ not "seen" ⇒ content re-inlined, `SeenTurn` left 0. Never
  hides changed bytes.
- **Lane divergence (what broke the test):** summary read and raw hit at one locator
  get *distinct* keys and are tracked independently — each dedups against its own
  prior-turn appearance. No lane clobbers another's fingerprint. Suppressing a raw
  hit because a *summary* of the same range was shown (different bytes) would itself
  be a bug; distinct keys avoid it.
- **True dedup still fires:** identical bytes across lanes/turns ⇒ identical key ⇒
  suppressed exactly as today.

Behaviour on a cross-turn repeat:
- bytes unchanged → `SeenTurn = firstTurn`, heavy field blanked (unchanged from today).
- bytes changed → key miss → re-inlined, `SeenTurn` stays 0 (looks like a fresh hit).

No new struct field ⇒ no `tool_schema_contract` golden change. Signalling the
change explicitly to the agent ("changed since turn N") would need a wire field and
is deferred to the delta follow-on; the correctness fix (stop hiding changed bytes)
lands with no schema churn.

## Edge cases

- **Empty content.** A metadata-only hit (`Content == ""`) fingerprints the empty
  string; there is nothing to suppress anyway. `SeenTurn` stamping is unaffected.
- **Same-turn duplicate lanes.** A locator surfaced twice in one turn is not a
  repeat (turn is not `< cur`); both render. With fp-in-key, identical-byte dupes
  share a key (recorded once), divergent-byte dupes get two keys (both recorded) —
  either way nothing is suppressed within a turn.
- **Truncation drift.** Per-read caps (`MaxLinesPerRead`/`MaxBytesPerRead`) are
  constant per inliner and applied upstream of `applySeenContext`, so unchanged
  content truncates identically across turns → stable fingerprint. Budget capping
  runs *after* the ledger (`context.go:626`), so it never perturbs the fingerprint.
  Worst realistic drift only ever *re-inlines* unchanged bytes (a rare, cheap
  over-send) — it can never *hide* changed bytes.
- **Memory.** One `map[string]int` entry per (locator, content-variant) per session;
  bounded by hits per session. Keys grow ~17 chars. Negligible.

## Interfaces

Unchanged public surface. Internal only:

```go
type seenState struct {
    turn  int
    first map[string]int // locatorKey + ":" + fingerprint → first-seen turn
}

func fingerprint(content string) uint64 // fnv64a
```

`note(path, start, end, content string)` gains the content arg; the three lane
loops pass their heavy field.

## Validation

- `TestSeenMarksRepeatsAcrossTurns` (existing) stays green — identical bytes across
  turns still dedup.
- New `TestSeenReinlinesChangedContent`: same locator, different bytes on turn 2 ⇒
  re-inlined, `SeenTurn == 0`; the changed bytes then repeated unchanged on turn 3 ⇒
  suppressed with `SeenTurn == 2`.
- New (or extended) case: two lanes, same locator, **different** bytes, same turn ⇒
  both render; on the next turn each dedups against its own bytes independently.
- `mooncake task ci-fast` green; no `tool_schema_contract` diff.

## Rollback

Single-file, no schema/state-format migration (the ledger is in-memory per
session). Revert `seen.go` + `seen_test.go`.
