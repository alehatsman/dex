---
id: memory-staleness
status: shipped
last_verified: ec521df
owners: [aleh]
covers:
  - "internal/store/store_knowledge.go"
  - "internal/mcp/server_knowledge.go"
  - "internal/mcp/referent.go"
  - "internal/mcp/remember.go"
  - "internal/store/store_search.go"
tracking: alehatsman/dex#167
---

# Memory staleness discipline: decay + liveness-check recalled facts

<!--
STATUS 2026-08-17 — ALL THREE PARTS SHIPPED, #167 closable.
- Part 2 liveness — shipped dbd0868 (annotateLiveness / referent.go).
- Part 1 decay taper — the recall scanner now ranks on max(updated_at,
  last_retrieved). `KnowledgeFact.LastRetrieved` populated by every scanFacts-fed
  recall SELECT; `scanFacts` Salience + `KnowledgeQueryVec` wRecency key on
  `lastTouched`. Deviation from draft: the SQL-ordered `KnowledgeQuery` LIMITs
  before Go scores, so the taper reorders where the consumer sorts by score
  (the semantic `KnowledgeQueryVec` path) and is metadata elsewhere; export /
  by-id / by-scope reads stay on updated_at (not recall rankers).
- Part 3 referent-overlap supersede — write-time nudge surfaces active facts
  sharing a code referent (file collapses line; symbol by name) the Jaccard
  word-overlap misses. Deviations from draft: (a) ZERO store changes — reused
  `KnowledgeExportAll`'s active-set scan (the ≤active-set in-memory scan the
  spec sanctioned) rather than a new LIKE-probe; (b) folded into the existing
  `Similar` list + a distinct hint clause ("already speak to <file>") instead of
  a sibling `Contradicts` list (minimal interface); (c) gated on
  supersedes_id==0 so a write that is already resolving isn't nagged.
-->


## Goal

A recalled fact that no longer holds is worse than no fact — the agent acts on
it with recall-time confidence. Three enforcement gaps, in trust-priority order:

1. **Liveness (Part 2, core)** — a fact naming a `path:line`, symbol, or env/flag
   whose referent no longer resolves against the current index must surface with
   a `needs_verification` flag, not as ground truth. This owns the measurement
   gate and is the precondition for any shared findings bus (#159).
2. **Decay taper (Part 1)** — untouched facts must sink in recall ranking instead
   of surfacing forever at write-time confidence. Partly built (`recencyFactor`);
   the gap is that the taper ignores `last_retrieved`.
3. **Contradiction surfacing (Part 3)** — a newly-written fact that overlaps an
   active one on the *same referent* should route to `supersedes`, not stack.

## Non-goals (from #167)

- **No LLM re-verification.** Liveness = deterministic index lookup only.
- **No auto-delete, no auto-confidence-change.** Downgrade/flag only; deletion
  stays an explicit `supersedes` / `gc`.
- No "opposing claim" semantic detection. Part 3's deterministic proxy is
  *shared referent* → surface for the agent to judge.

## Substrate that already exists

- `KnowledgeFact` carries `confidence`, `updated_at`, `hit_count`,
  `revision_count`, `valid_until`, `superseded_by`; the schema also has
  `last_retrieved` (`store_migrate.go`), *scanned by the GC decay pass but NOT by
  the recall scanner*.
- `scanFacts` (`store_knowledge.go:359`) computes `Salience = qualityWeight ×
  recencyFactor(UpdatedAt)`. `recencyFactor` (:332) is a linear 90-day taper —
  **keyed on `updated_at` only**. `KnowledgeQueryVec` (:852) folds a `wRecency`
  term with the same `UpdatedAt`-only recency.
- Liveness lookups: `(*Store).CodeFilePaths` (`store_search.go:1206`) → set of
  indexed code paths; `(*Store).FindSymbol(name,k)` (:973) → symbol resolution;
  `(*Store).SymbolsByFile` (`store_graph.go:448`) → path-scoped symbols.
- Write-path near-dup: `KnowledgeSimilar` (body Jaccard) → `rememberVerb` surfaces
  `Similar` and nudges `supersedes` (`remember.go:59`).
- `KnowledgeReview` (:1301) already emits read-only `stale`/`overlap` proposals;
  the session-start nudge consumes `.Total`.

## Design

### Part 2 — referent liveness (core)

**Referent extraction** — new `internal/knowledge` (or a `referent.go` in
`internal/mcp`) pure function:

```go
type Referent struct {
    Kind string // "path" | "pathline" | "symbol" | "env"
    Raw  string // as written, for the note text
    Path string // for path/pathline
    Line int    // for pathline (0 = none)
    Name string // for symbol
}
func ExtractReferents(body string) []Referent
```

Conservative, low-false-positive matchers (precision > recall — a missed referent
just means no liveness signal; a false one wrongly flags a good fact):

- **pathline**: `\b([\w./-]+\.[A-Za-z]{1,5}):(\d+)\b` → `Path` + `Line`.
- **path**: a token with a `/` and a known code extension, or a repo-rooted
  `internal/…` / `cmd/…` prefix. Backtick-quoted paths preferred.
- **symbol**: only high-signal forms — `(*Recv).Method`, `pkg.Exported`, or a
  backtick-quoted `CamelCase` identifier. Bare lowercase words are NOT symbols.
- **env** / flags (`--foo`) deferred — `env` liveness would need the
  `cmd/dex/env.go` registry (package `main`, not importable from
  `internal/mcp`) or a full-index search; neither is worth v1. Extract nothing
  for them.

**Liveness check at recall** — in `server_knowledge.go`, after `recallFacts`
returns and before envelope shaping (the `Server` holds both the knowledge store
and the code `*store.Store`, so this belongs in the mcp layer, not
`store_knowledge`):

```go
func (s *Server) checkLiveness(ctx, st, facts) // fills computed fields
```

Build the `CodeFilePaths` set once per recall (`map[path]maxEndLine`). For each
fact, for each extracted referent:
- `path` → present in the path set? Guard false-positives: only judge a path
  whose extension is one dex actually indexes (derive the indexed-extension set
  from the path map itself, self-calibrating) — a fact naming `go.mod` /
  `tasks.yml` stays unjudged rather than wrongly flagged.
- `pathline` → file present AND `line ≤ maxEndLine` (the map value is
  `MAX(end_line)`, so an out-of-range line is a dead referent for free — no file
  read needed).
- `symbol` → `FindSymbol(name,1)` returns ≥1 hit?

A fact is flagged **iff it has ≥1 referent of a kind AND every referent of that
kind fails to resolve** (all-fail, not any-fail — one live referent means the
fact is still anchored). Set two computed (non-persisted) fields:

```go
KnowledgeFact.NeedsVerification bool   `json:"needs_verification,omitempty"`
KnowledgeFact.VerificationNote  string `json:"verification_note,omitempty"`
```

Note e.g. `names internal/foo.go:42 which no longer resolves against HEAD`.
Surfaced in the `remember(recall)` and `ask` fact envelopes. **No mutation** of
`confidence`, `active`, or `valid_until`.

### Part 1 — last_retrieved-weighted taper

- Add `last_retrieved` to the recall column lists (`KnowledgeQuery`,
  `KnowledgeQueryArchetype`, `KnowledgeQueryVec`, `KnowledgeFactsMissingVec`) and
  scan into a new `KnowledgeFact.LastRetrieved time.Time`.
- `recencyFactor` keys off `max(UpdatedAt, LastRetrieved)` — "touched" =
  confirmed OR recalled. A fact retrieved recently fades slower (matches the
  GC-decay `last_retrieved` protection already at `store_knowledge.go:1093`);
  a genuinely untouched fact sinks on the same 90-day linear curve.
- `KnowledgeBump` (`store_knowledge.go:724`) already writes `last_retrieved=now`
  on every surfacing (and deliberately leaves `updated_at` alone) — the signal is
  fully populated. The only gap is the read side: the recall scanner drops the
  column and `recencyFactor` never sees it. No write-path change needed.

### Part 3 — referent-overlap supersede prompt on write

- Reuse `ExtractReferents` on the incoming fact body in the write path
  (`server_knowledge.go` add handler / `rememberVerb`).
- New store query: active facts whose body shares ≥1 referent with the new fact
  (cheap: extract referents of the new body, `LIKE`-probe or in-memory scan over
  the ≤1000-row active set — mirror `KnowledgeReview`'s O(n²)-over-small-store).
- Surface matches alongside the existing `Similar` list (or a sibling
  `Contradicts` list) with a `supersedes` nudge: "fact #N already speaks to
  `<referent>` — supersede it rather than stacking." Advisory only; the agent
  decides. Consistent with the "no opposing-claim semantics" non-goal.

## Build order

Part 2 → Part 3 (reuses the extractor) → Part 1 (independent). Part 2 first: it
owns the measurement gate and is the trust-unblocker. Each part lands as its own
commit; Part 1 can land in parallel.

**Status:** Part 2 shipped (`internal/mcp/referent.go` + liveness wiring in the
`knowledge` list/scope recall paths + a `remember(recall)` re-verify nudge).
Parts 1 and 3 remain — carried as follow-ups.

## Edge cases

- Fact with **no** extractable referent → never flagged (liveness is silent, not
  a downgrade). Most prose facts have no referent — that is fine.
- Empty / no index → skip liveness entirely (no path set to check against); facts
  surface unflagged rather than all-flagged.
- Pinned / `VerifiedFact` facts → still liveness-checked (a pin exempts from
  *decay*, not from *staleness reporting*); the flag is advisory, so no conflict.
- Symbol moved but same name elsewhere → resolves live (acceptable; name-level
  liveness, not identity-level — deterministic and cheap, matches non-goals).
- Renamed file with a stale `path:line` fact → correctly flagged (the win case).

## Validation

- Unit: `ExtractReferents` table test — pathline/path/symbol/env hits + the
  false-positive guards (bare lowercase word, prose colon-number).
- Unit: `checkLiveness` flags an all-dead-referent fact, leaves a live-referent
  and a no-referent fact unflagged.
- Unit: `recencyFactor` with `LastRetrieved` newer than `UpdatedAt` scores higher
  than with `UpdatedAt` alone.
- Unit: write with a referent already owned by an active fact surfaces the
  supersede nudge.
- **Measurement gate (#167):** a `dex notes audit` (or extended `notes review`)
  reporting *% of active facts whose named referent resolves against current
  HEAD* — run on this repo's own store before/after. Target: recalled packs
  contain zero unflagged dead-referent facts.
- `mooncake task ci` green (build + test + vet + fmt + race).
- Live: seed a fact naming a since-renamed path, `remember(query=…)` → the fact
  carries `needs_verification` + note.

## Out of scope (deferred)

- `env` (`DEX_*`) and flag (`--foo`) resolution — no importable/deterministic
  registry from `internal/mcp`; not worth a full-index search in v1.
- Auto-supersede / auto-delete on dead referents — stays explicit.
- Symbol *identity* liveness (moved but same name elsewhere resolves live) —
  name-level only, by design.
