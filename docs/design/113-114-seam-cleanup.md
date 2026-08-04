# 113 + 114 — finish the #95a mcp→retrieve seam cleanup

Status: in progress (2026-08-04)
Branch: `refactor/113-114-seam-cleanup`
Issues: #113 (relocate inline machinery), #114 (delete dead ask/context funcs)

Both are the deferred tail of #95a: assembly logic moved into
`retrieve.Assembler`, leaving trivial wire↔neutral adapters in `internal/mcp`.
The *policy* already lives in `retrieve`; the mcp funcs carry no logic of their
own. This branch drains the two remaining puddles. They are independent — landed
as two commits on one branch.

## Goal

- **#114**: delete three now-dead transport wrappers that the full golangci-lint
  gate flags `unused`, moving their unit coverage down onto the retrieve funcs
  they wrapped. CI `unused` count drops by 3.
- **#113**: move the assemble inline *orchestration* into `retrieve` so
  `Assembler.finish` runs it natively on `ContextPack`, dropping the injected
  `AssembleRequest.Inline` hook and the `inlineWirePack` wire round-trip.

Both are behavior-neutral: no wire output changes, same lane order, same caps.

## #114 — delete dead wrappers, port coverage

### Dead wrappers (no production caller; `retrieve.Assembler` calls the retrieve
funcs directly since #95a)

| Wrapper | File | Delegates to |
|---|---|---|
| `pickSuggestedReads` | `internal/mcp/context_results.go` | `retrieve.PickSuggestedReads` (+ injects `isNonImplPath`) |
| `enrichGraph` | `internal/mcp/context_graph.go` | `retrieve.EnrichGraph` |
| `(*Server).runSemanticLane` | `internal/mcp/context_lanes.go` | `retrieve.Service.SemanticLane` |

Each is a pure adapter: convert wire→neutral, delegate, convert back. Deleting
them loses only the *tests* that invoke them — and those tests are really
exercising the retrieve funcs through a trivial shim.

### Coverage port (mcp unit tests → retrieve, calling the funcs directly)

Retrieve already implements the policy but under-tests it (2 PickSuggestedReads
cases, 2 EnrichGraph arch cases). Port the mcp cases down, translating wire
types → retrieve types (identical field names) and supplying a test classifier
where the transport injected `isNonImplPath`:

- `internal/retrieve/results_test.go` ← the `pickSuggestedReads` cases
  (symbol-intent, cross-lane bias, doc-score-wins, code-preferred-over-build,
  PageRank tiebreaker subtests, rollup-summary filter, architecture cap).
  Classifier stub: `func(p string) bool` returning true for `.md`/doc and
  build/config paths the tiebreaker cases assert on.
- `internal/retrieve/graph_arch_test.go` (or a new `graph_caps_test.go`) ← the
  `enrichGraph` cap + PageRank-anchor cases, asserting on the returned
  `GraphResult.Nodes/Edges` instead of `out.Graph`.
- `internal/retrieve` (SemanticLane test) ← `TestLeanSemanticLaneRunsBM25`,
  using a `store.Searcher` stub calling `Service{}.SemanticLane` directly.

Integration tests that drive the live router (`ContextRouter*`) stay in mcp —
they exercise the real edge, not the dead wrappers.

### Edge cases
- `isReadableRange` / rollup-summary filtering lives in retrieve already — the
  ported test asserts it there.
- The lean-lane test asserts a nil query vector reaches `Search`; retrieve's
  `Service.SemanticLane` is the code under test, so the stub moves too.

## #113 — relocate inline orchestration into retrieve

### What moves (currently in `internal/mcp`, operating on wire types)
- `expandAssemblePool`, `coveringNode`, `nodeToSymbolHit`, `assembleInlinePrep`,
  `inlineWorkingSet` (`assemble.go`)
- `countInlinedBytes` (`context_response.go`)
- the `inlineContent` wrapper collapses — retrieve's `inlineWorkingSet` calls
  `retrieve.InlineContentKeyed` directly (no wire hop)

### What is dropped
- `inlineWirePack` (`context_enrich.go`) — the wire↔neutral round-trip
- `AssembleRequest.Inline func(*ContextPack)` hook — `Assembler.finish` calls the
  now-native inline pass directly
- `context.go:453` no longer injects the hook

### Reuse already in retrieve
- `retrieve.InlineContentKeyed`, `retrieve.AssembleKeywords`,
  `retrieve.AssembleConcerns` — the leaf policy is already here.
- `nodeToSymbolHit` composes Role via the Assembler's injected `FormatRole`
  (already threaded), not the mcp `formatRole` free function.

### Interfaces
- `Assembler` gains an unexported inline method (or `AssembleRequest` keeps
  `NoInline`, `ProjectRoot`, `Graph` which it already carries). The graph
  `*graphquery.View` is already on `AssembleRequest.Graph` — the pool widening
  needs it, so no new plumbing.
- Inline tests migrate to `internal/retrieve/inline_test.go` (where the caps
  tests already moved per the note in context_test.go:898), calling the native
  pass on neutral slices.

### Behavior-neutrality argument
Same functions, same order (widen pool → inlineContent → concerns), same caps
(`InlineCapsFor`), operating on the neutral twin of the same fields. The wire
projection (`context_project.go`) is unchanged. Only the *type the pass runs on*
changes (wire → neutral), removing two conversions per request.

## Validation
- `mooncake task test` green after each commit.
- `mooncake task ci-fast` (build + test + vet + fmt) green before push.
- Full-gate `unused` count drops by exactly 3 (the #114 wrappers).
- No diff in wire JSON for a sample `ask` (spot-check via a router integration
  test that already asserts inlined Content/Concerns).

## Commit plan
1. `refactor(mcp): #114 port dead-wrapper coverage into retrieve, delete wrappers`
2. `refactor(retrieve): #113 relocate inline orchestration, drop the Inline hook`
