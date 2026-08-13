---
id: pack-efficiency-bench
status: accepted
last_verified: efdf109
owners: [aleh]
covers:
  - "internal/bench/pack/pack.go"
  - "cmd/dex/bench_pack.go"
tracking: alehatsman/dex#163
---

# Pack-efficiency bench — prove the context pack is cheaper at held correctness

## Goal

Instrument epic #95's two open acceptance criteria with a deterministic,
gateable benchmark:

- **AC #2** — a common modify/fix workflow requires **materially fewer
  retrieval calls** than the primitive workflow.
- **AC #6** — benchmarks **demonstrate reduced tokens and tool calls without
  reducing task correctness**.

Both are the same measurement: the one-call `ask(intent=assemble)` context pack
vs the multi-call primitive path, on the **modify-symbol** task, credited only
where the pack does not lose coverage.

## The task regime — modify-symbol

To *safely* change a symbol `S`, an agent must assemble `S`'s working set:

- `S`'s definition (file + signature),
- its **callers** (edit ripples outward),
- its **callees** (edit depends inward),
- the **tests** that cover it,
- the local **rules** that govern its file (CLAUDE.md / owners / scoped notes).

This is exactly what `Assembler.Assemble` returns in a `ContextPack`
(`Symbols`, `Graph`, `RelatedFiles`, `Annotations.Tests/Owners`, `ScopedNotes`).
The bench measures the cost of *reaching that same working set* two ways.

## Two workflows

**Primitive** (no pack — raw lanes, one action per step):

1. `locate`/`search` S — 1 call, envelope tokens.
2. `read` S's file — 1 call, full-file tokens.
3. `trace` callers — 1 call.
4. `read` each distinct caller file — 1 call each, full-file tokens.
5. `trace` callees — 1 call.
6. `read` each distinct callee file — 1 call each, full-file tokens.
7. `find` + read tests — 1 call + full-file tokens.

Cost = Σ calls, Σ full-file read tokens + envelope tokens.

**Pack** (`ask(intent=assemble, S)`):

1. One call. Tokens = the assembled pack's rendered size (compressed
   signatures + rules + related paths), **not** full files.

## Ripple recall = the correctness floor (the AC #6 guard)

For each task, `gold` = the working-set files the primitive path must open
(def ∪ caller files ∪ callee files), derived from the call graph and bounded to
the common modify case (a symbol with ≤ `packGoldMax` neighbour files — a
30-caller god-object whose full ripple no single pack could surface is skipped,
per AC #2's "common workflow").

`coverage(task) = |gold ∩ pack_surfaced| / |gold|`, where `pack_surfaced` =
`Symbols[].Path ∪ Graph-lane neighbour files ∪ SuggestedReads ∪ References ∪
Annotations keys ∪ Annotations[].Tests`. The pack's graph lane names neighbour
*symbols*; they are resolved to files via the node table.

**Live calibration:** on dex's own repo the assemble pack surfaces a similarly
sized working set in one call but captures ~60% of the strict call-graph ripple
— full coverage is the exception, not the rule. So the cost delta is reported
over **reached** tasks (pack returned usable evidence), with **mean coverage
(ripple recall) reported alongside as the correctness floor**: a pack that is
cheap because it returned *less* shows up as lower recall, not a hidden gain.
The regression gate protects recall and full-cover rate (may not fall) and pack
cost (may not rise). This is the honest form of "without reducing correctness" —
correctness is measured and surfaced, not asserted.

## Interfaces

Pure package `internal/bench/pack`, decoupled from the retrieval stack (mirrors
`internal/bench/nav`):

```go
type Task struct {
    Symbol string   // qualified name of S
    Def    string   // S's own file (repo-relative)
    Gold   []string // def ∪ callers ∪ callees ∪ tests — the required working set
}

// CostModel prices the primitive actions; injected so the package needs no
// tokenizer or disk. Read = tokens to read one file; TraceEnvelope = tokens of
// a trace/locate result envelope the agent scans.
type CostModel struct {
    Read          func(path string) int
    TraceEnvelope func(paths []string) int
}

// PackModel injects the live pack outcome for one task: the files it surfaced
// and its rendered token cost — so the pure policy never touches the assembler.
type PackModel struct {
    Surfaced func(symbol string) (files []string, tokens int, ok bool)
}

type Result struct {
    Symbol             string
    PrimitiveCalls     int
    PrimitiveTokens    int
    PackCalls          int      // 1 on a hit
    PackTokens         int
    Coverage           float64  // |gold ∩ surfaced| / |gold|
    FullyCovered       bool     // coverage == 1.0
}

func Compute(tasks []Task, cost CostModel, pm PackModel) Report
func (r Report) Regressions(ref Report, absTol, relTol float64) []Regression
func (r Report) JSON() ([]byte, error)
func (r Report) Markdown() string
```

`Report` aggregates: NumTasks, NumCovered, mean/median primitive vs pack calls &
tokens **over fully-covered tasks**, MeanCoverage over all, and the deltas
(pack − primitive; negative = pack cheaper).

CLI `dex bench pack <project>`:

- opens the live index + graph,
- derives modify tasks from the highest-fan-in function/method symbols (gold =
  caller ∪ callee ∪ sibling-test files, from graph edges — same seed machinery
  as `buildBreadthTasks`),
- runs `Assembler.Assemble` per task to fill `PackModel.Surfaced`,
- prices reads with the file-token cost model (reuses `navCostModel`'s pricing),
- prints the report; `--check ref.json` gates regressions.

## Edge cases

- **Symbol with no callers/callees** — not a modify-ripple task; skipped
  (min neighbour floor, like nav's breadth seeds).
- **Pack miss** (`ok == false`) — coverage 0, excluded from the delta, counted
  against reach.
- **Test-file discovery** — sibling tests via `Annotations.Tests`; a gold test
  the pack omits lowers coverage (honest miss, not hidden).
- **Empty graph** (BM25-only index) — no gold derivable; CLI reports the reason
  and skips, like nav's map lane.

## Out of scope

- Live LLM correctness eval (task *success*): coverage is the correctness proxy
  here; end-to-end success stays with `internal/eval`.
- The other intents (`explain`, `debug`): modify-symbol is the AC's named
  "modify/fix workflow"; other intents are follow-ups.
- CI gate membership: like `bench nav`, this is a local-compute instrument, not
  part of `mooncake task ci`.

## Validation

- `internal/bench/pack` unit tests: hand-built tasks + fake cost/pack models
  assert calls, tokens, coverage, the fully-covered filter, and the regression
  gate — no index.
- `dex bench pack <dex-repo>` on the live index emits the headline delta;
  numbers committed as the reference and recorded in
  `docs/design/95-architecture.md` §7.
