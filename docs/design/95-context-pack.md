# Design — #95b/#95c ContextPack: the L2 domain schema and its wire projection

Status: **design / spec** · Child of [#95](95-architecture.md). Defines the pack
contract before any code moves. Validate against the code, then extract.

## TL;DR

`ContextPack` is the **domain** result of assembly — it lives in `internal/retrieve`
(L2), carries no JSON tags, and holds evidence + completeness + a **trust envelope**.
`mcp.ContextOutput` (L4) becomes a thin JSON-tagged projection of it. Nothing here is
new computation: every field already exists somewhere in the tree today; the spec's job
is to give the pack **one owner (L2)** and **one shape**, and to fold the six scattered
trust signals into a single envelope.

## 1. Why a domain type at all

Today `mcp.ContextOutput` (682-line `context.go`) is doing three jobs at once: it's the
assembly result, the trust carrier, and the wire schema. That fusion is why assembly
logic is stuck at L4 (§6.1 of the architecture doc). Splitting the domain pack out:

- lets the assembly funcs (`assembleConcerns`, `expandAssemblePool`) move to L2 where
  they belong, operating on domain types with no wire coupling;
- gives #95c a native home for confidence/freshness in L2, next to the facts;
- keeps `ContextOutput` as a stable, #93-pinned wire contract that just *serializes* the
  pack — wire changes and domain changes stop being the same edit.

Precedent already in the tree: `retrieve.SemHit`/`SuggestedRead` (domain, no tags) vs
`mcp.SemHit`/`SuggestedRead` (json-tagged twins). #95 completes that pattern.

## 2. The `ContextPack` schema (L2, `internal/retrieve/pack`)

Domain types — no `json` tags. Shown as Go for precision; the shape is the contract.

```go
// ContextPack is the assembled, intent-shaped working set for one ask.
type ContextPack struct {
    Intent   string          // resolved intent (ResolveIntent)
    Question string

    // --- Evidence lanes ---
    Symbols       []SymbolHit     // domain SymbolHit (moved down from mcp)
    SemanticHits  []SemHit        // existing retrieve.SemHit
    SuggestedReads []SuggestedRead // existing retrieve.SuggestedRead
    Graph         *GraphResult
    References    []RefHit
    RelatedFiles  []string        // spreading activation (#688), assemble only

    // --- Accumulated knowledge ---
    KnowledgeFacts []string       // top project facts by salience
    ScopedNotes    []Note         // gotcha-on-touch (#645)

    // --- Completeness (#725) ---
    Concerns Concerns             // Covered / Dropped

    // --- Trust envelope (#95c) ---
    Trust Trust

    // --- Cost accounting ---
    ContentBytesInlined int
    Expanded            bool      // query expansion contributed (#252)
}

// Trust folds the six signals dex already computes but scatters.
type Trust struct {
    // Freshness — is the index behind the working tree?
    Stale       bool       // ContextOutput.Stale today
    Indexing    bool       // a reindex is underway, results partial (#531)
    IndexedAt   time.Time  // index mtime; drives age

    // Confidence — how much to trust the ranking.
    TopScore    float32    // fused top semantic score
    LowConf     bool       // TopScore < LowConfidenceScore (0.45)
    // per-hit lane agreement already lives on SemHit.Lanes (#707)

    // Claims — proven graph facts vs heuristic edges.
    GraphResolved bool     // true when all graph edges are type-resolved (Go)
    RecallPartial bool     // name-based edges present → recall incomplete
    Caveat        string   // the name-based-recall warning (response.go:158)
}

type Concerns struct{ Covered, Dropped []string }
```

## 3. Field-by-field mapping — nothing is invented

Every pack field traces to something dex produces **today**. This table is the
validation: if a row has no "source today", it's speculative and must be cut.

| `ContextPack` field | Source today | → `ContextOutput` wire field |
|---|---|---|
| `Intent` | `ResolveIntent` (`retrieve/intent.go`) | `intent` |
| `Symbols` | `mcp.SymbolHit` (move domain part to L2) | `symbols` |
| `SemanticHits` | `retrieve.SemHit` (`service.go:47`) | `semantic_hits` |
| `SuggestedReads` | `retrieve.SuggestedRead` (`results.go:12`) | `suggested_reads` |
| `Graph` / `References` | `GraphResult` / `RefHit` | `graph` / `references` |
| `RelatedFiles` | spreading activation (#688) | `related_files` |
| `KnowledgeFacts` | `knowledge_facts` table | `knowledge_facts` |
| `ScopedNotes` | notes scope-bind (#645) | (via annotations) |
| `Concerns` | `AssembleConcerns` (#725) | `concerns` |
| `Trust.Stale` | `ContextOutput.Stale` | `stale` |
| `Trust.Indexing` | `ContextOutput.Indexing` (#531) | `indexing` |
| `Trust.TopScore`/`LowConf` | `retrieve.LowConfidenceScore=0.45` (`inline.go:25`) | *(new: surface it)* |
| `Trust.GraphResolved`/`RecallPartial` | `graphquery.EdgeKind` (`traverse.go:150`) | *(new: surface it)* |
| `Trust.Caveat` | recall warning (`response.go:158`) | `avoid` |
| `ContentBytesInlined` | existing | `content_bytes_inlined` |
| `Expanded` | query expansion (#252) | `expanded` |

**Read of the table:** the only genuinely *new* wire fields are two confidence signals
(`LowConf`, `GraphResolved`/`RecallPartial`) that dex already knows internally and
currently throws away before the response. That is the entire net-new surface of #95c.

## 4. The wire projection (L4, stays in `mcp`)

`mcp.ContextOutput` keeps its `json` tags and gains one method:

```go
func toContextOutput(p *retrieve.ContextPack) ContextOutput
```

All the assembly, concern-tagging, and trust computation happen in L2 and land on the
pack; `mcp` only maps pack → wire and applies wire-only concerns (status codes, hints,
`Answer`/`AnswerModel` from the chat step, session `Map`, seen-turn elision). The #93
golden contract test continues to pin `ContextOutput` — unchanged in spirit, one or two
new optional fields.

## 5. Extraction order (matches architecture doc §5)

1. **Define `ContextPack` + `Trust` in `internal/retrieve/pack`** (this spec) — no
   callers yet; pure type addition. *(#95b)*
2. **Move domain `SymbolHit` + assembly funcs down** to operate on the pack; `mcp`
   builds a pack, then projects. Behavior-neutral; the #93 contract test is the guardrail.
   *(#95a, now *after* the schema — the schema is the target it moves toward.)*
3. **Populate `Trust`** from the signals in §3 and surface the two new fields. Gate the
   wire additions behind the contract test's `-update`. *(#95c)*

## 6. Acceptance

- `ContextPack` is the single assembly return type in L2; `mcp` has no assembly logic,
  only projection.
- Every §3 row still maps to a real source (no speculative fields).
- Trust envelope carries freshness + confidence + proven-vs-heuristic in one place.
- #93 contract test passes with only additive wire changes.
- Measured: no regression in tokens/tool-calls per task vs the pre-refactor `ask`
  (behavior-neutral for #95a; #95c only *adds* signal).
