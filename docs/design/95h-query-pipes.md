# 95h — query pipes (MVP): composing lanes over the Selection currency

Status: **building** · Tracking: #206 (roadmap Phase 2) · Builds on: #207 (95f/95g), spec `specs/query-pipe.md`

## What this delivers

A single `query` input may compose lanes with `|`:

```
(*Server).query | callers | impact
/writeCurrentTask/ | callers
internal/store | callers | impact
how are edits debounced | callees | assemble:6000
```

Each `|`-segment is a **stage** run left-to-right, each consuming the prior
stage's refs. A length-1 pipe is today's behaviour exactly — purely additive:
the pipe path is entered only when the trimmed input has a top-level `|`.

## The one design decision the spec left open: which layer threads the currency

`specs/query-pipe.md` (written *before* #207 landed) put the keystone
`Selection`/`Ref` in `internal/retrieve` and threaded it between stages. #207
shipped differently in a way that makes the pipe simpler: it surfaced the
currency **on the wire** — `QueryOutput.Refs []Ref` (`query_refs.go`) is
populated by *every* lane dispatch (`refsFromExact`/`refsFromSemantic`).

So a pipe stage is just **a lane dispatch that already hands back its refs**.
Threading `retrieve.Selection` across stages would force a wire→domain→wire
conversion at every boundary for no gain. Decision:

> **The pipe composes at the wire layer.** The inter-stage currency is the mcp
> `[]Ref` (+ running trust, stages, budget), bundled in an unexported
> `pipeState`. `retrieve.Selection` remains the intra-lane *domain* twin #207
> built (embedded in `ContextPack`); it is not the pipe's hand-off type.

This respects the #95 domain/wire seam rather than fighting it. `pipeState` *is*
the Selection currency, just at L4 where the lanes actually produce it. (If a
future ref-driven transform needs to run *inside* a lane over domain refs, that
lane converts at its own boundary — same as today.)

## Execution model

```
pipe    := stage ( "|" stage )*
```

- Split on top-level `|`; a `|` inside a `/regex/` seed is not a separator.
- **Segment 0 is the seed:** run the existing single-lane path
  (`classifyQuery` → `dispatchExact`/`dispatchSemantic`) on it, unchanged. Its
  `QueryOutput.Refs` seed `pipeState.refs`; its `Trust`/`Status` seed the
  running envelope. `in.Kind` applies to the seed only.
- **Interior segments are transforms** (`Selection → Selection`): `callers`,
  `callees`, `impact`. Each **fans out** — runs its lane once per input ref,
  unions + dedupes results by `Ref.ID`, capped by `pipeMaxRefs`.
- **The last segment MAY be a terminal** (`Selection → body`): `signatures`,
  `assemble:N`, or `count`. If the last segment is a transform, the default
  terminal is that transform's own typed lane output over the unioned set.

### Fan-out + coercion

A transform needs `symbol` refs. When the input set is a different kind, coerce:

| input kind | coercion | source |
|---|---|---|
| `chunk` (grep/semantic hit `path:line`) | enclosing symbol | `locate` lane (existing surface method) |
| `file` (path seed) | contained top-level symbols | `SymbolsByFile` |
| `file` + dir path (e.g. `internal/store`) | exported symbols under dir | `ExportedSymbolsByDir` |

`chunk→symbol` rides the existing `locate` toolSurface method. `file/dir→symbol`
needs store access, exposed as an **optional capability** (`symbolCoercer`,
type-asserted like `seenLooker` in `look.go`) implemented on `*Server` — *not* a
new `toolSurface` method (keeps the 30-method interface clean; the remote surface
runs the whole pipe server-side on a `*Server` via `/query`, so it never needs
its own coercer). If the capability is absent and a coercion is required, the
pipe returns an honest error naming the failed coercion — that error-clustering
is the signal for which coercion to add next (spec Coercion policy).

Coercions are provenance-logged and never *raise* trust.

### Trust = weakest link

`pipeState.trust` starts at the seed's provenance and only ever weakens: a
semantic seed makes the whole pipe `semantic` even if later stages are exact; a
partial-recall trace or any coercion drags provenance down. This is the core
agent-calibration win (spec §Why 2).

### Budget

`pipeState.budget` seeds from `in.Budget` (or caps default) and each stage
debits. The `assemble:N` terminal caps its output at N tokens, dropping
lowest-score refs and recording the drop — an honest partial, never a silent
overflow (reuses the #164 clamp discipline).

## Envelope

- `QueryRoute.Stages []string` (omitempty) — the ordered segments executed,
  echoed so the agent learns the grammar in-band.
- `route.lane` = the terminal lane; `route.detected` = `"pipe"`.
- `trust.provenance` = weakest link (above).
- Everything else (`result`, `refs`, `cost`, `next`) unchanged; `refs` carries
  the final Selection so the agent can pipe further in a follow-up.

## MVP scope / deferred

**In:** `|` parse · seeds {path, dir, `/regex/`, prose, symbol} · transforms
{callers, callees, impact} · terminals {default, signatures, assemble:N, count}
· coercions {chunk→symbol, file→symbols, dir→symbols} · `route.stages` +
weakest-link trust.

**Deferred** (grow from observed use, per spec Non-goals): selector grammar
(`pkg:`/`func:`/`calls:`), LLM transforms, `path` transform in a pipe (needs a
`to:` per-ref target), branching/fan-in/named intermediates, CLI pipe surface,
and the full coercion matrix (everything not in the table errors honestly).

## Validation

1. **Correctness:** `A|B|C` yields the same refs as three manual round-trips
   (fixture unit test in `query_pipe_test.go`).
2. **Round-trip win:** tokens for one piped call vs N separate `query` calls —
   report it.
3. **Agent landing:** dogfood on dex.
4. `mooncake task ci` green (incl -race).
