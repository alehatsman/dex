# Design — #95g Query wire collapse (the flat lane envelope)

Status: **design / spec** · Child of [#95f](95f-selection-spine.md) · Tracking: **#207**
Decision: **clean-break the wire** (agent-only surface; owner controls the agents).

## TL;DR

`query`'s result is a union nested four deep:
`QueryOutput.Result.Look.Result.Read`. The `look`/`ask` wrapper adds a layer
that names nothing the `route.lane` field doesn't already name, and the `ask`
side is a 35-field god-struct (`ContextOutput`) serving five intents at once.
Collapse both: `QueryResult` becomes a **single flat discriminated union keyed
by `route.lane`**, and the semantic god-struct splits into per-intent wire
shapes. This CHANGES the JSON — the tool-schema contract golden is regenerated.

## 1. Today (the nesting)

```
QueryOutput{ Status, Route, Trust, Cost, Next,
  Result QueryResult{
    Look *LookOutput{ Status, Hint, Trust, Cost, Next,
      Result LookResult{ Kind, Read *SummarizeOutput, Grep *SearchGrepOutput,
                         Trace *TraceOutput, Locate *LocateOutput } }
    Ask  *ContextOutput{ …35 fields: envelope + semantic evidence + Map(orient)
                         + Review(review) … } } }
```

Two problems: (a) `Look` re-wraps a discriminator (`LookResult.Kind`) that
duplicates `route.lane`, and re-declares envelope fields (Status/Trust/Cost/Next)
that `QueryOutput` already hoists; (b) `Ask` is one struct for orient + review +
search + editing + assemble, mostly-`omitempty`, the reader can't tell which
fields co-occur.

## 2. Target (flat, lane-keyed)

`route.lane` is the single discriminator. `Result` holds exactly one populated
lane payload; envelope fields live once, on `QueryOutput`.

```go
type QueryResult struct {
    // exact lanes — the former LookResult, unwrapped (no LookOutput envelope)
    Read   *SummarizeOutput  `json:"read,omitempty"`
    Grep   *SearchGrepOutput `json:"grep,omitempty"`
    Trace  *TraceOutput      `json:"trace,omitempty"`
    Locate *LocateOutput     `json:"locate,omitempty"`

    // semantic lanes — the former ContextOutput, split per intent
    Semantic *SemanticResult `json:"semantic,omitempty"` // search/editing/assemble/architecture/packages
    Orient   *OrientResult   `json:"orient,omitempty"`   // session-start map
    Review   *ReviewOutput   `json:"review,omitempty"`   // review my changes
}
```

`route.lane` values map 1:1 onto the populated field: `read|grep|locate|trace|
semantic|orient|review`. (`symbol` — the input-shape name — routes to the
`trace` lane; see §4.)

### The semantic split

`ContextOutput`'s fields sort into three homes:

- **Envelope** (Status, Hint, Trust, Cost, Next, Project, Intent) → already on
  `QueryOutput`; dropped from the lane payload.
- **`SemanticResult`** — the evidence lanes: Answer, AnswerModel, SemanticHits,
  Symbols, Graph, SuggestedReads, References, Annotations, NextAction, Avoid,
  RelatedFiles, Rules, Concerns, ContentBytesInlined, Expanded, SessionTask,
  Endpoint. One coherent shape for the five evidence intents.
- **`OrientResult`** — `{ Map string }`, the deterministic session-start bundle.
  Its own type so orient's answer isn't a lone field on a 35-field struct.
- **`ReviewOutput`** — already a distinct type; promoted to a first-class lane.

## 3. Where the projection lives (L4, mcp only)

`ContextOutput` stays as the semantic router's **internal** assembled result
(`contextRouter` and its `toolSurface` signature are unchanged — no ripple into
http/remote/noop surfaces). The collapse is a **projection at the queryVerb
boundary**:

- `dispatchExact`: unwrap `LookOutput` → set the one exact lane on `QueryResult`
  directly; hoist its Trust/Cost/Next to `QueryOutput` (already done today).
- `dispatchSemantic`: project the internal `ContextOutput` into the lane keyed
  by its resolved intent — `orient` → `OrientResult`, `review` → `ReviewOutput`,
  everything else → `SemanticResult`.

`LookOutput`/`LookResult` become internal-only (lookVerb still returns them; only
queryVerb sees them) — or are inlined away if nothing else consumes them. Net:
mcp holds projection, not a second envelope.

## 4. Vocab

`route.lane` = `trace` for a symbol input (the operation is a call-graph trace).
`route.detected` = `symbol` (the input shape). `kind=symbol` stays as an input
alias that forces the trace lane. One operation name (`trace`) on the wire; the
`symbol` word survives only as an input-shape label and a `kind` alias — no
third spelling. `TraceInput`/`TraceOutput`/`traceVerb` keep their names.

## 5. Acceptance

- `QueryResult` is flat: seven lane pointers, exactly one populated, named by
  `route.lane`. No `look`/`ask` wrapper; no `LookResult.Kind` re-discriminator.
- Semantic god-struct split: `SemanticResult` + `OrientResult` + `ReviewOutput`;
  no single wire struct serves more than its intent family.
- Envelope fields (Status/Trust/Cost/Next) appear once, on `QueryOutput`.
- `tool_schema_contract` golden regenerated (`-update-tool-contract`); the change
  is intentional and reviewed, not silent.
- Every lane's payload bytes are unchanged from today (same SummarizeOutput,
  same evidence fields) — only the envelope nesting collapses.
- `contextRouter`/`ContextOutput` internal signature unchanged (bounded blast
  radius); the split is a projection, not an engine rewrite.
```
