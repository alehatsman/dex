# Design — #95f Selection: the universal lane currency (query spine)

Status: **design / spec** · Child of [#95](95-context-pack.md) · Tracking: **#207**
(roadmap Phase 1). Settles the keystone decision before implementation.

## TL;DR

`ContextPack` (#95b) gave the **semantic/assemble** lane a domain result with a
folded `Trust` envelope, projected to `mcp.ContextOutput` at L4. But it covers
**only 1 of 3 lane families**. `Selection` generalizes that seam to *every*
lane: a thin universal currency `{Refs, Trust, Stages, Budget}` that all lanes
emit and pipes (#206) thread. **`ContextPack` embeds `Selection`; it is not
replaced by it.** This is the universalization of an existing, closed, tested
seam — not a green-field IR.

## 1. The gap #95 left

The domain/wire seam is real but partial:

| Lane family | Domain type today | Wire type today | Seam |
|---|---|---|---|
| semantic / assemble | `retrieve.ContextPack` | `mcp.ContextOutput` | ✅ clean |
| orient / review | none | `mcp.ContextOutput` (built inline) | ❌ bypassed |
| read / grep / trace / locate | none | bespoke `*Output` structs | ❌ absent |

The symptoms this produces are the #207 smells: facade-over-facade
(`QueryResult{Look, Ask}`), `normalizeNext` string-rewriting dead verb names,
`symbol`/`trace`/`traceVerb` vocab drift, the 35-field `ContextOutput`
god-struct. All are what you get when two of three lane families have no domain
type and build wire structs inline. One fix — a universal currency — removes the
class.

> Note: `pack.go`'s doc comment ("no caller builds a ContextPack yet") is
> **stale** — `assembler.go` builds one today (#111). Truth-up is a Phase 0
> deliverable; this spec assumes the post-#111 reality.

## 2. `Selection` — the currency (L2, `internal/retrieve`)

Domain type, no `json` tags (same discipline as `ContextPack`).

```go
// Ref is one located code entity flowing through a lane or a pipe stage.
type Ref struct {
    Kind  string         // file | symbol | chunk | package | edge
    ID    string         // stable id: path, qualified symbol, node id
    Path  string
    Span  [2]int         // line range when applicable
    Prov  string         // exact | semantic | name-based | model
    Score float64
    Meta  map[string]any // lane-specific extras (edge kind, matched line, …)
}

// Selection is the uniform result every lane emits and every pipe stage
// consumes. Trust moves here from ContextPack — the "one trust envelope"
// (#95c) now lives where every lane can carry it.
type Selection struct {
    Refs   []Ref
    Trust  Trust    // folded envelope (freshness + confidence + proven-vs-heuristic)
    Stages []string // segments run (echoed to route.stages); length 1 today
    Budget int      // remaining token budget; each pipe stage debits (#206)
}
```

## 3. The decision: embed, not subsume

`ContextPack` **embeds** `Selection` and keeps its rich *typed* evidence lanes.

```go
type ContextPack struct {
    Selection                    // Refs + Trust + Stages + Budget

    Intent   string
    Question string

    // Rich typed evidence lanes — kept as-is (zero regression, #93 contract).
    Symbols        []SymbolHit
    SemanticHits   []SemHit
    SuggestedReads []SuggestedRead
    Graph          *GraphResult
    References      []RefHit
    RelatedFiles    []string
    Concerns        Concerns
    ContentBytesInlined int
    Expanded            bool
}
```

- `ContextPack.Refs` (from the embedded `Selection`) is a **flattened index
  over** the evidence — the handle pipe stages thread. It does not *replace* the
  typed lanes.
- Exact lanes produce a **bare `Selection`** (no `ContextPack`).
- `Trust` was on `ContextPack` (#95c); it moves onto the embedded `Selection`.
  Net: one envelope, now reachable from every lane.

**Why not subsume** (fold the typed lanes into `Ref.Meta`): `SuggestedRead`
(Content, Imports, Reason) and `SemHit` (Lanes, Score, Reason) carry typed,
tested rendering. Collapsing them into `map[string]any` discards that for no
gain and violates #95's own "nothing is invented, behavior-neutral" rule. `Refs`
is a new lightweight index *over* the evidence, not a replacement *for* it.

## 4. Wire projection (L4, `mcp`)

Every lane crosses one seam; mcp holds **no assembly logic, only projection** —
the same acceptance bar #95 set.

- `Selection → {signatures | grep | callgraph | slice | count}` wire outputs —
  the bespoke `SummarizeOutput`/`SearchGrepOutput`/`TraceOutput`/`LocateOutput`
  become terminal projections *from* a `Selection`, not parallel types.
- `ContextPack → ContextOutput` — the existing #95 projection, unchanged; `#93`
  golden contract test stays the guardrail (additive only).
- `normalizeNext` deleted: lanes emit `query` verb names natively.
- Vocab unified: one name for `symbol`/`trace` across all layers.

## 5. Serve requirement (Phase 0b keeps `dex serve`)

Today the wire surface exposes the *old primitives* (`/ask`, `/lookup`, …) and
the remote shim composes `query` **client-side** — several REST round-trips per
call, chatty over a container network (moongit). Because every lane now returns
a serializable `Selection`, `query` becomes a **first-class server-side
endpoint**: one round-trip per call, and later one round-trip for a whole
`a | b | c` pipe (#206). `Selection` needs a json-tagged wire twin at L4 (same
domain/wire twin pattern as `SemHit`).

## 6. What this unlocks (pipes, #206)

- **Transforms** (`callers`/`callees`/`impact`/`filter`) are `Selection →
  Selection`, threading `Refs`. They already exist as single-symbol lanes; the
  pipe adds fan-out (run per ref, union) + coercion.
- **The terminal split falls out naturally:** *structural* terminals
  (signatures/grep/callgraph/slice) render from any `Selection`; the *rich
  assemble* terminal only applies at length-1 semantic, where the `ContextPack`
  (not just its `Selection`) survives intact.

## 7. Acceptance

- `Selection` is the return currency of every lane; `ContextPack` embeds it.
- mcp builds no wire struct inline; every lane projects `Selection → envelope`.
- `Trust` lives once, on `Selection`; reachable from every lane.
- `normalizeNext` gone; one vocabulary; god-struct split into per-intent
  projections.
- `#93` golden contract test green (additive wire changes only).
- `query` is a first-class serializable wire endpoint.
- No regression in tokens/tool-calls per task vs the pre-refactor `query`.
