# Spec: anti-accretion lint (#139)

Status: proposed
Epic: #110 tool-surface cutover, step 10 (standing guard)
Issue: #139

## Goal

Keep the tool surface from re-accreting the frankenstein it took epic #110 to
untangle. When tools compete for the agent's attention their descriptions start
negotiating with each other — "use X instead", "when Y is overkill", two tools
both claiming to be the "primary entry point". Those phrases are a *smell*: a
clean surface tells each tool's own story and lets the router pick, so no
description needs to argue against a sibling. This lint is the standing guard the
migration plan names (`specs/tool-surface.md`, step 10 / Validation).

## Scope

- In: one new test in `internal/mcp` that enumerates advertised tool
  descriptions and asserts a ceiling on sibling-negotiation offenses.
- Out: rewording existing descriptions to drive the ceiling to zero (that is a
  separate cleanup slice, one description at a time, ratcheting the ceiling
  down); any wire-schema or behavior change; prose beyond the offense patterns
  below.

## Background — what we can enumerate

`listToolSchemas(t, srv)` (in `tool_schema_contract_test.go`) already does a real
`ListTools` round-trip and returns `[]*sdk.Tool`, each carrying `Name` and
`Description`. The schema-contract test walks all three registration profiles
(default / expert / lean). The lint reuses that same enumeration — descriptions,
not schemas.

## Design — a ratchet, mirroring the router-accuracy floor

The router-accuracy harness (`router_accuracy_test.go`) sets a floor just below
the measured baseline and documents its honest gaps rather than pretending they
don't exist. This lint is the same idea, inverted: a **ceiling** on offense count
that sits at the measured baseline, so no change may add *new* negotiation
phrasing, and every cleanup ratchets the ceiling down toward zero.

Offense patterns (case-insensitive, scanned over each tool's `Description`):

1. **Redirection** — a description that tells the agent to reach for a *different*
   tool, anchored on the `use <tok> instead` idiom. Anchored deliberately: a bare
   `instead of` fires on legitimate prose ("surface it on touch *instead of* it
   leaking into chat" redirects nothing), so the pattern requires the "use …
   instead" shape to count.
2. **Comparison** — a description that measures itself against a sibling's weight:
   `overkill` (as in "when brief is overkill" / "a context pack is overkill").
3. **Contested primacy** — `primary entry(-)point` / `primary entrypoint`
   appearing on **more than one** tool. One front door is fine; two tools both
   claiming primacy is negotiation. Counted only for the excess (count − 1).

Measured baseline (default profile, 12 tools) = **1**: the `ask` description
negotiates with `brief` ("prefer brief for coding tasks; use ask where a context
pack is overkill"). That single offense is retired by construction in step 4 (the
ask-merge folds `brief` into an `ask` intent — no sibling left to negotiate
with), so `antiAccretionCeiling` drops to 0 then. `brief`'s lone "PRIMARY
ENTRYPOINT" is not an offense: one front door is the intended shape.

Offenses are summed across the *default* profile (the surface the agent sees by
default; expert/lean are supersets/subsets and would double-count shared tools).
The test logs each offending `tool: phrase` so the debt is visible, then asserts
`offenses <= antiAccretionCeiling`.

`antiAccretionCeiling` is a single named constant set to the measured baseline.
Lowering it is the whole point; raising it requires a reviewed reason in the
commit, exactly like moving the router floor.

## Edge cases

- **Self-reference is allowed.** A tool naming *itself* ("brief(task)…") is not an
  offense; only cross-tool redirection/comparison counts. The patterns require a
  sibling token, and the primacy rule triggers only on >1 distinct tool.
- **MCP-instructions prose is out of scope.** The server's MCP *instructions*
  block (the `dex is active…` preamble) legitimately maps native→dex tools; this
  lint scans per-tool `Description` strings only, not that preamble.
- **Profiles.** Default profile only, to avoid counting a shared tool three times.

## Interfaces

Test-only. No public surface, no wire schema, no golden. One constant
`antiAccretionCeiling` and one `TestAntiAccretionLint`.

## Validation

- `TestAntiAccretionLint` green at the baseline ceiling; offenders logged.
- `mooncake task ci-fast` green; no `tool_schema_contract` diff (descriptions are
  not part of that golden).

## Rollback

Single test file. Revert it.
