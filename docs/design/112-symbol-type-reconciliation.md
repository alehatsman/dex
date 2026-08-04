# 112 — Reconcile the two neutral symbol types (SymHit vs SymbolHit)

Status: draft · Part of #95 · follows #95a (docs/design/95a-assembly-domain-seam.md)

## Goal

Collapse `internal/retrieve`'s two neutral symbol types into one, so pack
assembly stops shuttling symbols between two shapes. Behavior-neutral; a
clarity/maintenance win, not a feature.

## Problem

`internal/retrieve` carries two transport-free symbol types:

- **`SymHit`** (`service.go:69`) — lean lane row. Carries `Name` + the four
  raw centrality columns (`InDegree/OutDegree/CrossPkgCallers/Betweenness`)
  plus the shared 8 (`QualifiedName Path StartLine EndLine Kind Signature Body
  Truncated`). Produced by `SymbolLane`; read by the prose builders
  (`response.go`), `inline.go`, `results.go`, `graph.go`.
- **`SymbolHit`** (`pack.go:78`) — rich pack twin (field-for-field mirror of
  the wire `mcp.SymbolHit`, no json tags). Adds `Doc Role Handle SeenTurn`;
  drops `Name` + centrality. Held by `ContextPack.Symbols`; read/filled by
  `enrich.go` and the assembler.

`Assembler.Assemble` builds the pack with `toSymbolHits` (SymHit→SymbolHit,
formatting Role via the injected `FormatRole`), then `finish()` calls
`packSymHits` (SymbolHit→SymHit) to hand the prose builders a SymHit slice —
a lossy round-trip that exists only because the two halves of one lifecycle
speak different types.

## Why one type is safe (behavior-neutrality argument)

The only reason the split looked load-bearing is the #95a design note: `SymHit`
holds *raw* centrality rather than a formatted `Role` so the transport owns the
role-display vocabulary (injected `FormatRole`). A single type **preserves
this**: it carries both the raw centrality columns *and* a `Role` field that
stays empty until the injected `FormatRole` runs in the assembler. Nothing moves
down; the policy stays injected.

Verified field reads (grep, whole repo):
- The four centrality columns are **written only in `internal/graph/centrality.go`**
  and **read only by `FormatRole`** at the assembler edge. No prose/inline/graph
  lane reads them.
- `Name` is consumed only as `FormatRole`'s first argument.
- The prose/inline/results lanes read only shared fields (`QualifiedName Path
  Signature Body Kind StartLine EndLine Truncated`).

So merging to a superset type cannot change behavior: the extra fields each
consumer now sees (centrality on the pack side; `Doc/Role/Handle/SeenTurn` on the
lane side) are simply unread by lanes that didn't read them before. The lossy
round-trip (`toSymbolHits`/`packSymHits`) that currently drops centrality on the
way in and `Doc` on the way out disappears — but since no downstream reads those
dropped fields, output is byte-identical.

## Decision

**Survivor: `retrieve.SymbolHit`. Delete `retrieve.SymHit`.**

- Naming: restores the domain-twin convention already used for `RefHit`,
  `PathMeta`, `SemHit` (same name across `mcp` and `retrieve`). `SymHit` is the
  lone odd name; deleting it aligns symbol with sem (`mcp.SemHit`↔`retrieve.SemHit`).
- The merged `retrieve.SymbolHit` absorbs `SymHit`'s `Name` + four centrality
  columns as lane-time input fields, documented as "consumed by the injected
  `FormatRole`; zero after Role is formatted."

Rejected alternative — *embed a lean core in the rich type*: keeps two names and
the conversion surface; the issue explicitly prefers "one type." Rejected
alternative — *keep both, document the layering*: the issue lists this as the
fallback only; the round-trip converter is the smell, and one type removes it
outright.

## Scope

### `internal/retrieve`
- `service.go` — delete `type SymHit`; move its `Name` + centrality fields onto
  `SymbolHit` in `pack.go` (documented as FormatRole inputs). `SymbolLane`
  returns `[]SymbolHit`.
- `assembler.go` — delete `packSymHits` and the SymHit round-trip; `finish()`
  passes `pack.Symbols` (`[]SymbolHit`) straight to the prose builders.
  `toSymbolHits` becomes an in-place Role-format pass over `[]SymbolHit` (or
  folds into `SymbolLane` + a `formatRoles` step).
- Signature flips `[]SymHit`→`[]SymbolHit`: `response.go` (`BuildNextAction`,
  `BuildAvoid`, `AssembleConcerns`, `AssembleNextActionHint`, `distinctSymbolPaths`,
  `firstInlinedAnchor`, `buildSymbolLookupNextAction`, `buildCallerCalleeNextAction`),
  `inline.go` (`InlineContent`, `InlineContentKeyed`, `coverageOrder`,
  `fillSymbolBodies`, `fillSymbolBodiesOrdered`, `CountInlinedBytes`),
  `results.go` (`PickSuggestedReads`), `graph.go` (`EnrichGraph` + `graphEnricher.symbols`).
- Tests: `response_assemble_test.go`, `response_prose_test.go`, `results_test.go`,
  `submodular_test.go` — `[]SymHit{…}`→`[]SymbolHit{…}` (mechanical).

### `internal/mcp` (boundary)
- `context_convert.go` — `toNeutralSyms` (wire→SymHit) collapses into the single
  wire→`retrieve.SymbolHit` converter; confirm/redirect its call sites.
- `context_enrich.go` `toPackSyms` / `context_project.go` `fromPackSyms` stay
  (they cross the genuine L4 json boundary) — the wire `mcp.SymbolHit` keeps its
  json tags and its own identity. The mcp wire type is **out of scope**: this
  issue is the two *neutral* types only.

### #114 is decoupled (do NOT fold in)
Original plan folded #114's dead funcs (`enrichGraph`, `pickSuggestedReads`,
`runSemanticLane`) in as commit 1. On inspection that premise doesn't hold:
- The "avoid churning dead signatures" reason is weak — all three take the
  **wire** `mcp.SymbolHit` (unchanged by #112) and delegate through
  `toNeutralSyms` (the one converter #112 touches anyway). #112 needs nothing
  from #114.
- They are thin transport wrappers with **no production caller**, but each
  carries real test coverage that is **not mirrored in `retrieve`**:
  `pickSuggestedReads` (~15 intent-routing cases in `context_test.go` vs 2 in
  `results_test.go`), `enrichGraph` (4 cap cases, partial overlap in
  `graph_arch_test.go`), `runSemanticLane` (lean BM25-only fallback tested
  *only* via the wrapper in `lean_ask_test.go` — no retrieve `SemanticLane` test).
  So #114 is really "port the coverage down to `retrieve`, then delete the
  wrapper" — that is #114's own work, not a prerequisite here.

Decision: keep the wrappers untouched (they compile unchanged after the flip —
`toNeutralSyms` retargets to the merged type and `EnrichGraph`/`PickSuggestedReads`
accept `[]SymbolHit`). #114 stays a separate, better-scoped follow-up.

## Edge cases
- `SymbolLane` currently returns `([]SymHit, map[string]struct{})` — the paths
  map is unaffected; only the slice element type changes.
- `toSymbolHits` today allocates a fresh `[]SymbolHit` and formats Role. After
  the merge, Role formatting must still run exactly once, at the same point
  (post-symbol-lane, pre-pack-store) so `FormatRole`'s inputs (centrality) are
  present and its output ordering is unchanged.
- The test/doc/fixture demotion `sort.SliceStable` in `Assemble` runs on the
  lane rows before Role formatting — keep that order (sort, then format, then
  store) so Role values land on the same rows.
- `packSymHits` dropped `Doc`; prose builders never read `Doc`, so removing the
  drop is inert. Confirm no prose builder starts depending on a now-present field.

## Interfaces (after)
```go
// retrieve/pack.go — the one neutral symbol type.
type SymbolHit struct {
    QualifiedName string
    Name          string // raw symbol name; FormatRole input (zero after format)
    Path          string
    StartLine     int
    EndLine       int
    Kind          string
    // Centrality columns — FormatRole inputs only; unread by lanes/prose.
    InDegree        int
    OutDegree       int
    CrossPkgCallers int
    Betweenness     float64
    Signature string
    Doc       string
    Body      string
    Role      string // formatted by injected FormatRole in the assembler
    Truncated bool
    Handle    string // #344
    SeenTurn  int    // #344
}
// SymHit: deleted.
```

## Validation
- `mooncake task ci-fast` green (build + test + vet + fmt-check) — the existing
  retrieve/mcp suites are the behavior-neutrality gate.
- `mooncake task ci` — no *new* lint issues vs the main baseline (the 9 known
  pre-existing items from #111 stay, nothing added).
- Grep sweep: zero remaining `SymHit` identifiers (type gone), and
  `packSymHits`/round-trip converter removed.
- Spot-check wire output: an `ask`/`brief` response over a fixture is
  byte-identical before/after (symbol ordering + Role strings unchanged).

## Commit plan (one branch, worktree; #114 excluded)
1. `refactor(retrieve): #112 merge SymHit into SymbolHit` (type + lane + assembler)
2. `refactor(retrieve): #112 flip prose/inline/results/graph to SymbolHit` (+ tests)
3. `refactor(mcp): #112 retarget toNeutralSyms to the merged type`
