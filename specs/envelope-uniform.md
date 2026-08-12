# Uniform envelope + session spine (#110 steps 2–3)

## Goal
Finish the two partial steps of epic #110 so every verb response carries the
same top-level envelope shape and the session spine is consistent:

```
{ result…,
  trust:  { provenance, fresh, indexed_at, confidence, caveat, …evidence },
  cost:   { tokens_returned, saved_pct?, budget_left? },
  next:   [ {verb, args, why} ],
  handles:[ … ],          // where the verb returns fetchable content
  seen:   { … } }         // where the verb returns content that can repeat
```

## Current reality (validated 2026-08-12, main @5d4c5ef)
- Two trust types: `EnvTrust` (look/act/remember: `{provenance,fresh,indexed_at,confidence,caveat}`)
  and `trustEnvelope` (ask: `{stale,indexing,indexed_at,confidence,top_score,low_confidence,graph_resolved,recall_partial,caveat}`).
  **Same key (`trust`), different shape** — violates "same shape on every response."
- `EnvCost` type already has `{tokens_returned, saved_pct, budget_left}` but is
  populated **nowhere meaningfully**: only `act` sets `saved_pct`; `tokens_returned`
  and `budget_left` are never set. ask reports `content_bytes_inlined` instead.
- `next` is uniform (`[]NextStep`) and present where a grounded step exists.
- `handles` are rich on ask (per-hit encoded handles); absent elsewhere (look is
  itself the fetch target, so it needs none).
- `seen` (session dedup) exists on ask's hit types; fires only on repeat content
  within one persistent session. Not present on look/act/remember.

## Design

### 1. One trust type (EnvTrust as the superset)
Retire `trustEnvelope`. Extend `EnvTrust` with the ask-only evidence signals as
omitempty so exact verbs still project to `{provenance:"exact"}`:

```go
type EnvTrust struct {
    Provenance string  `json:"provenance"`            // exact | semantic | name-based
    Fresh      *bool   `json:"fresh,omitempty"`
    IndexedAt  string  `json:"indexed_at,omitempty"`
    Confidence string  `json:"confidence,omitempty"`  // high|medium|low
    Caveat     string  `json:"caveat,omitempty"`
    // evidence signals — set by ask (semantic provenance) only:
    TopScore      float32 `json:"top_score,omitempty"`
    LowConfidence bool    `json:"low_confidence,omitempty"`
    GraphResolved bool    `json:"graph_resolved,omitempty"`
    RecallPartial bool    `json:"recall_partial,omitempty"`
}
```

ask maps `retrieve.Trust` → `EnvTrust` with `Provenance:"semantic"`,
`Fresh = !Stale && !Indexing`. The old `stale`/`indexing` booleans collapse into
`fresh` + the caveat (no information lost: `indexing` surfaces via caveat when set).
ContextOutput.Trust becomes `*EnvTrust`.

**No information regression:** every trustEnvelope field maps forward, except the
two-bool `stale`/`indexing` fold into one `fresh` + caveat. Confirmed the only
callers of the removed fields are the ask response and its tests.

### 2. cost.tokens_returned everywhere; budget_left resolved
`tokens_returned` — dex **can** know this: count tokens of the serialized result.
Set on every verb via a single stamp at the response edge (one helper, applied in
each verb handler after the payload is built). Reuse `internal/tokens`.

**CONFLICT — `budget_left`.** The epic drew `cost.budget_left`, but dex has no
knowledge of the agent's context budget; it is not computable server-side.
Resolution (surfaced, not silently picked): `budget_left` is populated **only when
the caller supplies an optional `budget` input** (`budget_left = budget - tokens_returned`,
floored at 0); omitted otherwise. This keeps the field honest — present exactly
when it has meaning — without bloating the common call path (the param is optional
and unset by default). `saved_pct` stays where a verb compresses (act, look-read, ask).

### 3. Session spine — consistent, not padded
**CONFLICT — "mandatory on every response."** `seen` marks content the agent was
already shown this session so it isn't re-sent. That notion applies only to
**content-returning verbs**: ask (per-hit) and look (fetched bytes). It is
meaningless for `act` (runs a command — no dedupable content) and for
`remember` in write mode (persists a fact). Forcing an empty `seen` onto those is
exactly the padding verbs_envelope.go's omitempty design set out to avoid.

Resolution: the spine is **consistent where it applies**, not literally on every
response. look gains seen-turn dedup on its read/grep lanes (same fingerprint
mechanism ask uses); ask keeps its per-hit seen; act and remember-write omit it
(nothing to dedup). This honors the intent ("never re-send what the agent already
has") without noise. remember-recall (which returns facts) MAY carry seen later —
deferred, low value (facts are short).

### 4. handles
Unchanged: ask keeps per-hit handles (the progressive-disclosure mechanism). look
already returns exact bytes (it is the fetch target). No new work; documented so
the contract is explicit that `handles` is a per-hit field, not a top-level array.

## Non-goals
- A session-level budget model (per-call optional `budget` only).
- Forcing `seen`/`cost`/`handles` onto verbs where they carry no information.
- Changing retrieval quality, intents, or the verb set.

## Validation
- Full `mooncake task ci` green (gofmt + golangci-lint + test).
- Live re-dogfood over stdio: ask/look/act/remember all show one `trust` shape;
  `tokens_returned` set on each; `budget_left` appears iff `budget` passed; look
  emits seen on a repeat read within one session.
- Update `internal/mcp/testdata/tool_schema_contract.json` golden if input schemas
  change (the new optional `budget` param).

## Increments (checkpoint after each)
1. **Trust unification** — EnvTrust superset; ask → EnvTrust; delete trustEnvelope. (no conflict)
2. **cost.tokens_returned** — stamp on all four; optional `budget` → budget_left.
3. **Session spine** — look read/grep seen-turn dedup.
4. **Validate + close #110.**
