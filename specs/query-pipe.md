# Spec — `query` pipes: composing lanes into one call

Status: **draft / design** · Owner: TBD · Tracking: #206

## Goal

Let a single `query` input compose multiple lanes with `|`, so an agent gets a
multi-step retrieval in **one round-trip** instead of N:

```
query("internal/store | callers | impact")
query("how are edits debounced | callees | assemble:6000")
query("/TODO\(perf\)/ | enclosing:func | owners")
```

Each `|`-segment is a **stage**. Stages run left-to-right, each consuming the
prior stage's result. A pipe of length 1 is today's behavior exactly — this is
purely additive.

This is the jQuery insight applied to code intelligence: dex already has the
single shape-dispatched entry point (`query(input)` = `$(x)`); pipes add the
**composition over a uniform result set** that made `$('.x').filter().find()`
the thing every dev reached for. dex is **100% an agent tool** (decided) — so
the win is measured in round-trips and provenance, not human keystrokes.

## Why (agent-first justification)

1. **Round-trip collapse.** Every separate `query` call is tool-call overhead +
   re-established context + another decode. A pipe collapses a chain into one
   call. This generalizes the proven #163 pack-efficiency result (−88.9% calls /
   −85% tokens for `assemble`) from one fixed pipeline to arbitrary composition.
2. **Provenance through the chain.** The agent's core failure is trusting a fuzzy
   result as exact. A pipe that seeds semantic (fuzzy) then walks exact edges
   reports `trust` as the **weakest link across stages**, calibrating the agent
   automatically. No human tool needed this; an agent tool lives on it.
3. **The envelope teaches the grammar.** `route.stages` echoes what actually ran;
   `next[]` suggests continuations (`"add | impact"`). The agent learns the
   vocabulary in-band, one call at a time — never from docs.

## Non-goals (explicitly deferred — grow as we learn, per decision #3)

- **Selector grammar** (`pkg:store func:*Handler calls:embed`) — phase 2. MVP
  seeds stay shape-inferred (path / `/regex/` / symbol / prose), unchanged.
- **LLM-backed transforms** (`explain`, `summarize`, `classify`, `rerank`) —
  phase 3. The "pluggable local LLM" dynamic lanes. MVP transforms are 100%
  deterministic graph/index ops.
- **Full coercion matrix.** MVP ships the two obvious coercions and errors
  honestly on the rest (see Coercion policy). The matrix grows from observed
  agent errors, not up front.
- **Human/CLI pipe surface.** Agent-only. No `dex query "a | b"` CLI ergonomics
  work beyond what falls out for free.
- **Branching / fan-in / named intermediates.** Linear pipes only.

## Grammar

```
pipe    := stage ( "|" stage )*
stage   := seed | transform | terminal
```

- Split the raw input on top-level `|`, trim each segment.
- **Segment 1 is always a seed** (produces the initial `Selection` from a
  string, via the existing shape classifier).
- **Interior segments are transforms** (`Selection → Selection`).
- **The last segment MAY be a terminal** (`Selection → projection`); if it is a
  transform (or the pipe is length 1 with no explicit terminal), the default
  terminal is inferred from the final `Selection`'s dominant `Kind` — the
  shape-inference behavior, preserved.
- `|` inside a `/regex/` seed is not a separator (regex is delimited by `/`).

Parsing lives at the top of `queryVerb` (`internal/mcp/query.go`): if the
trimmed input contains a top-level `|`, route to the new pipe executor;
otherwise fall through to today's single-lane path untouched.

## Execution model

### The uniform set (the keystone)

Every stage produces and consumes one internal type. The agent never sees it;
it is the inter-stage hand-off. Lives in `internal/retrieve`.

```go
// Ref is one code entity flowing through a pipe.
type Ref struct {
    Kind  string         // file | symbol | chunk | package | edge
    ID    string         // stable id: path, qualified symbol, node id
    Path  string
    Span  [2]int         // line range when applicable
    Prov  string         // exact | semantic | name-based | model
    Score float64
    Meta  map[string]any // carried context so terminals need not re-fetch
}

// Selection is the wrapped set threaded between stages.
type Selection struct {
    Refs    []Ref
    Trust   Trust    // aggregate; provenance = weakest link seen so far
    Stages  []string // segments run, echoed into route.stages
    Budget  int      // remaining token/among budget; each stage debits
}
```

### Three stage signatures

- **Seed** `string → Selection`: reuses the existing classifier + lane.
  - path → file refs · `/regex/` → chunk refs · symbol → symbol ref (+1-hop) ·
    prose → semantic hit refs.
  - A prose seed still calls `writeCurrentTask` (the #610 adaptive-compression
    task signal) — preserved.
- **Transform** `Selection → Selection`: `callers callees impact path enclosing
  imports importers filter limit`. Most already exist as forced lanes behind
  `lookVerb` (`kind=callers|callees|impact|path`) — the pipe **fans out**: run
  the lane per input ref, union + dedupe the results into the next `Selection`.
- **Terminal** `Selection → body`: `signatures`(default) `skeleton map slice
  pack assemble:N count graph`. Projects the final set into the existing
  envelope `result` payload.

### Fan-out + coercion (the genuinely new machinery)

Today `callers`/`impact` take a *single* symbol string. A pipe stage carries a
*set*, and the set's `Kind` may not match what the transform needs
(`internal/store` seeds *files*; `callers` needs *symbols*). Two mechanisms:

- **Fan-out:** a transform runs once per input ref; results are unioned and
  deduped by `ID`. Per-stage cap (reuse `MaxGraphNodes`) bounds the blast.
- **Coercion:** when a transform needs kind X and gets kind Y, apply a coercion
  rule if one exists, else error honestly. MVP ships exactly two:
  - `file → symbols` (a path's contained top-level symbols)
  - `chunk → enclosing symbol`

  Coercions are provenance-logged (they never *raise* trust). Everything else is
  an honest error with a `next[]` suggestion — that error-clustering is the
  signal for which coercion to add next.

### Budget flows through

`Selection.Budget` seeds from `budget_tokens` (default from caps) at stage 1 and
each stage debits. An interior transform that would blow the budget truncates
(keeping highest-`Score` refs) and records the drop in `Concerns{Dropped}` —
an honest partial, never a silent overflow. This reuses the existing
`capsAssembleDense` clamp discipline (#164).

## Envelope changes

Minimal, backward-compatible:

- `QueryRoute` (`internal/mcp/query.go`) gains `Stages []string` (`omitempty`) —
  the ordered segments actually executed.
- `trust.provenance` reflects the **weakest link** across stages (a semantic
  seed makes the whole pipe `semantic` even if later stages are exact).
- `next[]` suggests one continuation stage where obvious (e.g. after `callers`,
  suggest `| impact`).
- Everything else (`result`, `cost`, `handles`) unchanged.

## Validated integration points

- Entry: `queryVerb` in `internal/mcp/query.go:229` — pipe split goes at the top,
  before `classifyQuery`.
- Exact/graph lanes (seeds + `callers`/`callees`/`impact`/`path` transforms):
  behind `lookVerb`.
- Semantic seed: behind `contextRouter` → `ResolveIntent`
  (`internal/retrieve/intent.go`).
- Route echo: `QueryRoute` (`internal/mcp/query.go:54`).
- Graph fan-out caps already exist (`MaxGraphNodes`/`MaxGraphEdges`).

## MVP scope (the first cut)

Just enough to run two real pipes end-to-end:

- Top-level `|` parsing in `queryVerb`.
- `Selection`/`Ref` IR in `internal/retrieve`.
- **3 seeds** (path, `/regex/`, prose — adapt existing lanes to emit `Refs`).
- **2 transforms** (`callers`, `impact`) with fan-out.
- **2 terminals** (`signatures` default, `assemble:N`).
- **1 coercion** (`file|chunk → enclosing symbol`).
- `route.stages` + weakest-link `trust`.

Runnable targets:
```
internal/store | callers | impact
how are edits debounced | callees | assemble:6000   (callees added if trivial)
```

## Validation / what we measure

The go/no-go is empirical (decision #3):

1. **Correctness:** `A | B | C` yields the same refs as three manual round-trips
   through the same lanes (unit test with a fixed fixture graph).
2. **Round-trip win:** total tokens for `pkg | callers | impact` in one call vs
   three separate `query` calls — expect a large drop (fewer envelopes, no
   re-inlined context). Report it.
3. **Agent landing:** does the pipe put the right context in one shot? Dogfood on
   dex itself.
4. `mooncake task ci` green.

If the two MVP pipes feel obviously better than N calls → invest in the full
`Ref` IR, grow transforms/coercions, then consider the selector grammar (phase
2) and LLM transforms (phase 3). If forced → we spent a couple days, not a
quarter.

## Risks

- **Combinatorial stage semantics.** Mitigated by the uniform `Ref` IR +
  coercion rules — never N×M hand-coded stage pairs.
- **Intermediate-set explosion** mid-pipe. Mitigated by per-stage caps + the
  flowing budget.
- **Losing shape-inference charm.** Mitigated: length-1 no-terminal pipe ==
  today's exact behavior; default terminal inferred from final kind.
- **Malformed pipes from the agent.** Forgiving parser + teaching envelope
  (`route.stages` shows what ran; error carries a `next[]` fix).

## Phasing

This spec is **Phase 2** of the roadmap (`specs/roadmap.md`). It is **blocked by
#207** (the query spine): the `Ref`/`Selection` currency this pipe threads
between stages is built there, not here. Do not start until #207 lands — on
today's un-collapsed facades there is no common currency to compose.

- **Prerequisite — #207 (roadmap Phase 1):** collapse `look`/`ask` facades;
  every lane emits `Selection`. After this the pipe MVP is mechanical.
- **This spec (roadmap Phase 2):** `|` parsing + fan-out + coercion + terminals
  over the `Selection` currency #207 provides.
- **Later (roadmap Phase 3):** selector-grammar seeds (`pkg:` `func:` `calls:`),
  more transforms/coercions driven by observed errors, then LLM-backed dynamic
  transforms (pluggable local model, provenance=`model`).
