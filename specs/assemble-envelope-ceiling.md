---
id: assemble-envelope-ceiling
status: accepted
last_verified: f7be786
owners: [aleh]
covers:
  - "internal/retrieve/policy.go"
  - "internal/mcp/context.go"
tracking: alehatsman/dex#164
---

# assemble must not overflow the client tool-result limit

## Goal

`ask(intent=assemble)` on a high-fan-in symbol can return a response that
exceeds the MCP client's tool-result token cap and **hard-errors** the whole
call (observed: 50,328 chars for `(*Server).recallFacts`, rejected by the
harness, payload dumped to a file). The one-call working set must always come
back bounded, degrading gracefully instead of erroring.

## Root cause (grounded, not guessed)

The failing 50 KB payload broke down as: `symbols` 24 KB + `semantic_hits`
12 KB + `suggested_reads` 5 KB ≈ 41 KB of inlined content (≈ the 40 KB
`capsDense.TotalBytesCap`), then `graph` 6.6 KB + misc ≈ 3 KB on top → ~50 KB.

The `clampResponseEnvelope` safety net (#784) already exists, but its ceiling is
`InlineCapsFor(intent).TotalBytesCap + 10 KB headroom`. For assemble that is
`40 KB + 10 KB = 50 KB` — sitting exactly at the harness reject point, so the
clamp trims *to* a ceiling that is itself over the limit.

assemble is uniquely exposed: it is the **only** intent that pairs `capsDense`
(40 KB pool) with `BodyFillCoverage` (submodular body-packing that fills the
pool with many symbol bodies). `architecture` / `package_topology` also take
`capsDense` but use `BodyFillNone`, so they do not pack bodies and have not
overflowed in practice.

`ContextInput.Budget` is report-only (it feeds `cost.budget_left`); it is not
plumbed into the assemble selection, so it cannot be relied on to bound size.

## Design

Give assemble its own inline cap, right-sized so its auto-derived envelope
ceiling stays under the harness limit with margin, while leaving room for its
graph (the callers/callees that make an assemble useful for a modify task):

```go
// policy.go
capsAssembleDense = InlineCaps{MaxLinesPerRead: 120, MaxBytesPerRead: 8 * 1024, TotalBytesCap: 28 * 1024}
// IntentAssemble.InlineCaps: capsDense → capsAssembleDense
```

`28 KB` matches the empirically-good run (`content_bytes_inlined` 28,428, a
14-body set covering 26/27 concerns), so the working set stays usable. The
existing `envelopeCeilingBytes` formula then yields `28 + 10 = 38 KB` for
assemble — a ~24% margin under the observed 50 KB failure — and the existing
clamp enforces it (shedding graph edges first, then tail content, on the rare
maximal assemble).

No change to `envelopeCeilingBytes`, the clamp, or the other intents: the pool→
ceiling invariant does the work.

## Out of scope

- Making `Budget` causal (shrinking the pool when a caller passes a budget) —
  a separate enhancement; the default ceiling fully resolves the hard-error.
- `architecture` / `package_topology` share the 50 KB ceiling and are a latent
  version of the same risk, but use `BodyFillNone` and have no reported
  overflow; treat as a follow-up if one surfaces.

## Validation

- `internal/mcp/context_envelope_test.go`: assert `envelopeCeilingBytes(assemble)`
  is (a) above its own pool (headroom invariant) and (b) safely below the
  observed 50 KB reject.
- `internal/retrieve`: assert `capsAssembleDense.TotalBytesCap < capsDense.TotalBytesCap`.
- Re-run the live repro (`ask intent=assemble "(*Server).recallFacts"` with no
  budget) and confirm it returns a bounded set instead of erroring.
