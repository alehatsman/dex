# Design — #95d/#104 EvidencePolicy: one table for per-intent evidence selection

Status: **design / spec** · Child of [#95](95-architecture.md) §5 step 3, WS8.
Scope decided with owner: **consolidate the existing 8 intents' evidence
selection into one table** — behavior-neutral. The aspirational intents in #104's
body (bug_fix/security/performance) do not exist in `ResolveIntent` and are
deferred; the table is the extension point where they would later plug in.

## Goal

Per-intent evidence selection is real today but **smeared across 4 functions in 3
files**, each re-deriving the same intent buckets in its own `switch`:

| Function | File | Selects per-intent |
|---|---|---|
| `(*graphEnricher).runForIntent` | `retrieve/graph.go:382` | which **graph lane** runs |
| `InlineCapsFor` + body-fill switch | `retrieve/inline.go:41,95` | inline byte/line caps + symbol-body fill mode |
| `PickSuggestedReads` | `retrieve/results.go:57` | max suggested reads |
| `answerMaxTokensFor` | `retrieve/answer.go:37` | answer synthesis budget |

The bucketing is duplicated: "architecture/package_topology are the dense
exploration tier" is asserted independently in three places. Changing one intent's
evidence budget means finding every switch. This spec gives the intent→evidence
mapping **one owner and one shape** — data, not scattered branches.

Out of scope (not evidence, response-shaping prose): `buildNextAction`,
`buildAvoid` (`response.go`), `assembleNextActionHint` (`mcp/assemble.go`),
`intentPayloadStrong`. These stay as-is.

## The `EvidencePolicy` table (L2, `internal/retrieve/policy.go`)

```go
// GraphLane names the graph-expansion mix an intent anchors on. The lane
// *implementation* stays in graph.go; the table only maps intent → lane.
type GraphLane int
const (
    GraphLaneNeighborhoodRollup GraphLane = iota // default: symbol nbhd + pkg rollup
    GraphLaneNeighborhood                         // symbol neighborhood only (no rollup)
    GraphLaneCallersInbound
    GraphLaneCalleesOutbound
    GraphLaneArchitecture
    GraphLanePackageTopology
)

// BodyFill names the symbol-body inline strategy during InlineContent.
type BodyFill int
const (
    BodyFillNone     BodyFill = iota
    BodyFillSymbols               // symbol_lookup: fillSymbolBodies
    BodyFillCoverage              // assemble: submodular coverage order (#687)
)

// EvidencePolicy is the per-intent evidence budget — the single source of
// truth the four selection sites read from.
type EvidencePolicy struct {
    GraphLane       GraphLane
    InlineCaps      InlineCaps // existing struct (inline.go)
    BodyFill        BodyFill
    MaxReads        int
    AnswerMaxTokens int
}

func PolicyFor(intent string) EvidencePolicy // map lookup, default fallback
```

## The table — every cell traces to a current switch arm

Dense caps = `{120 lines, 8KB/read, 40KB total}`; targeted = `{60, 4KB, 20KB}`
(verbatim from `InlineCapsFor`).

| Intent | GraphLane | Caps | BodyFill | MaxReads | AnsTok |
|---|---|---|---|---|---|
| `symbol_lookup` | Neighborhood | targeted | Symbols | 2 | 400 |
| `editing_context` | Neighborhood | targeted | None | 2 | 400 |
| `behavior_search` | Neighborhood | targeted | None | 2 | 400 |
| `callers` | CallersInbound | targeted | None | 2 | 400 |
| `callees` | CalleesOutbound | targeted | None | 2 | 400 |
| `architecture` | Architecture | dense | None | 5 | 900 |
| `package_topology` | PackageTopology | dense | None | 5 | 900 |
| `assemble` | NeighborhoodRollup | dense | Coverage | 2 | 400 |
| *(default/auto)* | NeighborhoodRollup | targeted | None | 2 | 400 |

Validation of behavior-neutrality:
- **GraphLane:** `symbol_lookup`+`editing_context`→neighborhood; `behavior_search`→
  neighborhood (rollup omitted deliberately, graph.go:414); `callers`/`callees`→
  calls-expansion with the empty-result neighborhood fallback kept in the lane impl;
  `architecture`→anchor-pkg rollup+import edges; `package_topology`→topology;
  `assemble`+unknown→neighborhood+rollup (the old `default:`).
- **Caps:** dense for architecture/package_topology/**assemble**, else targeted —
  exactly `InlineCapsFor`.
- **MaxReads:** 5 for architecture/package_topology, else 2 — exactly
  `PickSuggestedReads`.
- **AnswerMaxTokens:** 900 for architecture/package_topology, else 400 — exactly
  `answerMaxTokensFor`.

## Rewire (each site becomes a table read; lane impls unchanged)

- `runForIntent` → `switch PolicyFor(intent).GraphLane` (dispatch to the same methods).
- `InlineCapsFor(intent)` → `return PolicyFor(intent).InlineCaps` (kept as a shim so
  external call sites don't change).
- body-fill switch in `InlineContentKeyed` → `switch PolicyFor(intent).BodyFill`.
- `PickSuggestedReads` maxReads → `PolicyFor(intent).MaxReads`.
- `answerMaxTokensFor(intent)` → `return PolicyFor(intent).AnswerMaxTokens`.

## Acceptance

- One `EvidencePolicy` table is the single source of truth; the four sites read
  from it, no independent intent buckets remain.
- Table-driven test pins every intent → its full policy row (the intent→lanes
  contract #104 asks for), incl. the default fallback for `auto`/unknown.
- Behavior-neutral: existing `retrieve` tests (graph/inline/results/answer) pass
  unchanged. No token/tool-call regression — this consolidates, it does not retune
  (retuning is a follow-up now that there's one dial to turn).
