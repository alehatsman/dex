# 95c — Wire the Trust envelope onto the Assembler

Status: **done** · Child of [#95](95-architecture.md) §7 (confidence/trust
envelope) · issue #115 · builds on the #95a assembly seam and the #101 `Trust`
shape (retrieve/pack.go:52).

## TL;DR

The #95a Assembler builds every `ContextPack` field **except** `Trust` — the one
conspicuous hole. `Trust` is defined (pack.go:52) but never populated in
non-test code and never projected to the wire. This spec fills it at the domain
seam and surfaces it, folding signals dex already computes but currently
scatters across `internal/mcp` and `internal/retrieve`.

Behavior-neutral on every existing field; the only wire change is an additive,
`omitempty` `trust` object on `ContextOutput`.

## Where each signal lives today

| Trust field | Source today | Home for #95c |
|---|---|---|
| `Stale` | `contextRouterCheckStale` (context.go:298) → `out.Stale` | inject via request |
| `Indexing` | `indexingNotice` (context.go:305) → `out.Indexing` | inject via request |
| `IndexedAt` | `st.Stats().LastIndex` (computed, then dropped) | inject via request |
| `TopScore` | `maxSemScore(pack.SemanticHits)` (assembler.go:137) | Assembler.finish |
| `LowConf` | `LowConfidenceScore` (inline.go:25), threshold applied ad-hoc | Assembler.finish |
| `GraphResolved` | not computed today (new) | Assembler.finish |
| `RecallPartial` | approximated intent-only in `BuildAvoid` (response.go:179) | Assembler.finish |
| `Caveat` | recall string in `BuildAvoid` (response.go:179) | Assembler.finish |

Two axes of ownership, following the #95a seam:

- **Evidence-derived** (`TopScore`, `LowConf`, `GraphResolved`, `RecallPartial`,
  `Caveat`) — the Assembler has all the inputs in `finish` already. Compute there.
- **Freshness** (`Stale`, `Indexing`, `IndexedAt`) — store metadata, not
  retrieval. `store.Searcher` (what the Assembler holds) deliberately omits
  `Stats`/`IndexingInProgress` (searcher.go:11); do **not** widen it. The
  transport already computes freshness in `contextRouterCheckStale` before it
  builds the request — inject it.

## Interfaces

### `retrieve.AssembleRequest` — three freshness fields (injected)

```go
// Freshness — index vs working tree. The transport computes these (store
// metadata, not retrieval); the domain core only stamps them onto Trust so the
// pack is the single home for the envelope.
Stale     bool
Indexing  bool
IndexedAt time.Time
```

### `retrieve.Assembler` — stamp Trust

Freshness is stamped in `Assemble` right after `pack` is constructed (so it is
present even on the empty-lane early-return path — a stale index is often *why*
a result is empty). Evidence fields are stamped in a new `stampTrust` step at
the end of `finish`, reusing `topSem`/`graphEdges` already computed there:

```go
// in Assemble, after pack := ContextPack{...}
pack.Trust.Stale, pack.Trust.Indexing, pack.Trust.IndexedAt = req.Stale, req.Indexing, req.IndexedAt

// in finish, after topSem is computed
pack.Trust.TopScore = topSem
pack.Trust.LowConf = topSem > 0 && topSem < LowConfidenceScore
if pack.Graph != nil {
    pack.Trust.GraphResolved = pack.Graph.Resolved
    pack.Trust.RecallPartial = pack.Graph.RecallPartial
}
if pack.Trust.RecallPartial {
    pack.Trust.Caveat = RecallCaveat
}
```

### Resolution tracked in the graph enricher (retrieve/graph.go)

The surfaced `GraphResult` node IDs are **CompactID**s (graph.go:126), not raw
view IDs, so they can't be mapped back into `view.NodesByID` after the fact. The
one place with both the raw edge and the language is `addEdge`, which already
looks up `view.NodesByID[ge.SrcID/DstID]`. Track resolution there, scoped to the
edges actually surfaced (after the dedup/cap gate):

```go
// graphEnricher gains two fields:
sawCallEdge   bool // any EdgeCalls surfaced → a resolved claim exists to judge
nameBasedCall bool // a surfaced call edge touches a non-Go (tree-sitter) node

// in addEdge, after the edge is committed (past dedup/cap):
if ge.Kind == graph.EdgeCalls {
    e.sawCallEdge = true
    if !isGoNode(e.view, ge.SrcID) || !isGoNode(e.view, ge.DstID) {
        e.nameBasedCall = true
    }
}

// isGoNode: endpoint present in the view AND Go. A missing/non-Go endpoint is
// treated as name-based (an unresolved edge is not a type-resolved claim).
func isGoNode(view *graphquery.View, id string) bool {
    n, ok := view.NodesByID[id]
    return ok && n.Language() == "go"
}
```

`EnrichGraph` stamps the summary onto two new **domain-only** `GraphResult`
fields (not projected to the wire GraphResult, which stays Nodes+Edges):

```go
// GraphResult:
Resolved      bool // all surfaced call edges are Go (type-resolved)
RecallPartial bool // a surfaced call edge is name-based → recall incomplete

// EnrichGraph, before returning e.gr:
e.gr.Resolved = e.sawCallEdge && !e.nameBasedCall
e.gr.RecallPartial = e.nameBasedCall
```

The Assembler then reads these into Trust (`pack.Graph != nil && ...`).

`RecallCaveat` is a new const in response.go carrying the concise recall
warning (substance of BuildAvoid:179, but gated on the *computed* signal rather
than intent alone — strictly more honest). `BuildAvoid` is left untouched, so
`pack.Avoid` is byte-neutral.

### Wire — additive `trust` object on `ContextOutput`

```go
type trustEnvelope struct {
    Stale         bool    `json:"stale,omitempty"`
    Indexing      bool    `json:"indexing,omitempty"`
    IndexedAt     string  `json:"indexed_at,omitempty"` // RFC3339
    TopScore      float32 `json:"top_score,omitempty"`
    LowConfidence bool    `json:"low_confidence,omitempty"`
    GraphResolved bool    `json:"graph_resolved,omitempty"`
    RecallPartial bool    `json:"recall_partial,omitempty"`
    Caveat        string  `json:"caveat,omitempty"`
}
// ContextOutput:
Trust *trustEnvelope `json:"trust,omitempty"`
```

Projected in context_project.go via `fromPackTrust(pack.Trust)`, which returns
`nil` when the envelope is entirely zero (keeps empty responses byte-neutral).

## Edge wiring (context.go)

`contextRouterCheckStale` gains a `time.Time` return (its already-computed
`stats.LastIndex`); it keeps setting `out.Stale`/`out.Indexing`/`out.Hint`
exactly as before. The caller threads the three facts into the request:

```go
indexedAt := contextRouterCheckStale(ctx, st, &out, p.Root)
// ...
}.Assemble(ctx, st, retrieve.AssembleRequest{
    ...
    Stale: out.Stale, Indexing: out.Indexing, IndexedAt: indexedAt,
})
```

## Edge cases

- **Empty result** (no lane hits, early return): freshness stamped, evidence
  fields zero. Correct — no confidence claim, but staleness still explains empty.
- **No graph indexed** (`req.Graph == nil`): `GraphResolved=false`,
  `RecallPartial=false`, no Caveat. Absence of a claim, not a negative claim.
- **Graph with no call edges** (e.g. architecture intent, imports only):
  `GraphResolved=false` — nothing to resolve.
- **BM25-only / `DEX_EMBED_ENGINE=none`**: `TopScore` reflects the fused BM25
  score; `LowConf` threshold still applies. No special-casing.
- **Zero envelope**: `trust` key omitted entirely (omitempty + nil projection).

## Non-goals / follow-ups

- ~~Removing the legacy top-level `out.Stale`/`out.Indexing`~~ and ~~folding
  `Confidence` into Trust~~ — **done in #116**: the `trust` object is now the
  single home; the top-level `stale`/`indexing`/`confidence` fields are gone.
- Per-edge confidence scoring / weighting beyond the Go-vs-name-based split.

## Validation

- `mooncake task ci` (build + test + vet + fmt) green.
- New retrieve tests: `graphRecall` (all-Go→resolved, mixed→partial, no-graph,
  no-call-edges); Trust stamping on `Assemble` (freshness present on empty path;
  TopScore/LowConf thresholds; Caveat gated on RecallPartial).
- New mcp test: `fromPackTrust` projection (zero→nil; populated→wire object;
  IndexedAt RFC3339).
- Behavior-neutrality: every existing context_test / envelope assertion still
  green (no existing field changes; `trust` is additive omitempty).
