# 111 — apply the #95a assembler seam to the remaining verbs

Child of #95 · continues #95a (`retrieve.Assembler`, the `ask` domain core).

## Why

#95a lifted `ask` assembly into `internal/retrieve` (`Assembler` over a neutral
`ContextPack`, transport-owned policy injected as funcs) and reduced the `ask`
router to decode → call → project. The remaining read verbs still fuse their
ranking/traversal core with transport concerns inside `*Server`. #111 applies the
same seam so the domain core is one testable, transport-free unit per verb.

## Reality check — the five verbs are not uniform

| Verb | Today | Seam |
|------|-------|------|
| **search** | Ranking pipeline (`SearchFused → symbol RRF → spreading activation → cross-encoder rerank → ECS rerank → multi-scale filter`) fused with transport (throttle, index/staleness, loop-detect, SLO, handle-stamp, hint build) in `server_search.go`. | **The real lift.** Move ranking into `retrieve.SearchAssembler`; inject the TF-IDF multi-scale filter (server holds its index cache). |
| **locate** | Symbol resolution + composed enrichment legs. | Medium — legs already shared with the enricher. |
| **verify** | Test selection + resolve. | Low-medium. |
| **brief** | Already a composition of other verbs (`taskMap`/`Search`/`briefReviewPack`). | Low — its core is orchestration. |
| **trace** | Already pure dispatch (`traceVerb` decode → graph handler → fold). | Very low — near-target already. |

So #111 is one real seam (search), two medium (locate, verify), two already close
(brief, trace). Staged, one behavior-neutral commit per verb, gated by
`mooncake task ci-fast`. Reassess trace/brief after the first cuts — they may not
be worth the churn.

## Stage 1 — search (this cut)

`retrieve.SearchAssembler{Service}` owns the ranking core. `SearchRequest` carries
the resolved inputs (query, query embedding, candidateK, session task); the
transport-owned TF-IDF path filter is injected as `MultiScale func([]store.Hit)
[]store.Hit` (the server holds the multi-scale index cache), mirroring how
`Assembler` injects `FormatRole`/`IsNonImpl`/`Inline`.

`Assemble` returns the ranked `[]store.Hit` (through multi-scale). It does **not**
own: the query embedding (transport surfaces `ErrUnreachable`/endpoint), index +
staleness checks, language/`path_glob` filtering + k-truncation, loop-detect, SLO,
handle stamping, hint composition, and the `SearchHit` wire projection. Stage
errors are prefixed at the seam (`search:`/`rerank:`) so the transport's hint text
stays byte-identical.

The symbol-leg helpers (`collectSearchSymbolLeg`, `collectSymbolHits`) move from
`internal/mcp` into `retrieve` alongside the assembler.

### Validation

Behavior-neutral: same lane order, same candidateK-vs-k split, identical output.
Existing tests are the gate (`server_search*`, `render_sort_score`, `verb_parity`,
`clamp_searchk`, `search_throttle`, `search_filter`). `mooncake task ci-fast` green.
