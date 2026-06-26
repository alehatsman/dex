---
id: semantic-search
status: living
last_verified: 621894f
owners: [aleh]
covers:
  - "internal/rerank/**"
  - "internal/store/store.go"
  - "internal/store/store_graph.go"
  - "internal/retrieve/rerank.go"
  - "internal/mcp/server.go"
---
# Semantic Search

## Intent

Semantic search is dex's primary query surface: given a natural-language or
code query, it returns the handful of code chunks most relevant to it, ranked
well enough that an agent can read the top hits instead of grepping blind. It
embeds the query, retrieves nearest chunks from the vector index, fuses that
with a lexical (FTS/BM25) signal so exact identifiers aren't lost, and reranks
the merged pool for final ordering. Every stage beyond the embedding call fails
soft: a missing reranker or FTS leg degrades the result rather than the request,
and an offline embedding backend returns a distinct condition so the caller
(Claude) can fall back to grep. This spec covers the retrieve→fuse→rerank query
path; building the index and the on-disk engine are sibling specs'.

## Behavior

- WHEN a query is searched, dex embeds the query text, runs a nearest-neighbour
  search over the vector index, and returns the top-k chunks each carrying its
  file path, kind, name, start/end line, snippet content, and a cosine score.
- WHEN the query text is non-empty and the lexical leg is enabled, dex also runs
  an FTS5/BM25 search and fuses the two ranked lists via Reciprocal Rank Fusion
  (`1/(k+rank)`, k=60) so a hit strong in either leg can surface; the fused RRF
  rank decides ordering.
- WHERE the query text is empty or the lexical leg is disabled, search degrades
  to semantic-only and orders by cosine similarity.
- WHEN both legs run, dex pulls a wider candidate pool per leg than the final k
  so fusion has headroom to surface lexical-only or semantic-only hits.
- WHEN the call graph is indexed, dex adds a third graph-proximity RRF lane:
  files that are graph-neighbors of already-scoring files are retrieved and
  fused at `DEX_GRAPH_WEIGHT × γ^hop` weight (default 1.0 × 0.6 = 0.6× for a
  1-hop neighbor), so callers/callees of a hit can surface without the agent
  explicitly querying the graph. Raise `DEX_GRAPH_WEIGHT` (e.g. to 2–4) to make
  the lane compete more strongly with dense+BM25; tune with
  `dex bench eval --mode blast-radius`.
- WHERE BM25 is weighted, the path column is scored at 2× the body column
  (`bm25(chunks_fts, 1.0, 2.0, 0.5)`) so path-bearing queries surface the
  right file before its contents.
- AFTER RRF fusion (and before any cross-encoder rerank), dex runs a local
  reranking pass: test/fixture paths are penalized 0.3×; chunks whose `kind`
  indicates a definition are boosted 1.5× when the query contains identifiers;
  chunks from files with ≥2 hits receive a 1.15× coherence boost; and chunks
  beyond the second from the same file are decayed by 0.7× per excess (MMR
  diversity). This pass operates on RRF scores and degrades to cosine scores
  for semantic-only results.
- WHEN the same query is issued ≥4 times within 5 minutes, search appends a
  hint advising the caller to store the finding in `notes` rather than
  repeating the search.
- IF the FTS query is malformed (e.g. unbalanced quotes), dex falls back to the
  semantic-only ranking rather than failing the search.
- WHEN a reranker is configured and the fused pool is larger than k, dex reranks
  the pool with a cross-encoder and returns the top-k by rerank score; each hit
  carries its rerank score alongside the cosine/BM25/RRF scores.
- IF the reranker is unreachable, times out, or its circuit breaker is open, dex
  silently returns the pre-rerank (RRF) ordering — a reranker outage never
  surfaces as a search failure.
- WHILE reranking, dex applies a per-call deadline and memoizes results in an
  in-process cache keyed on the query plus the candidate id-set, so repeated
  queries in a session skip the network call.
- WHILE the reranker returns consecutive unreachable errors, a circuit breaker
  trips after a threshold and short-circuits further calls for a cooldown window,
  so a downed endpoint stops adding latency to every query.
- WHEN a per-file diversity cap is configured, dex limits hits per unique file
  path while preserving the score-based order.
- WHEN a result count is requested, dex returns at most k hits; the surface
  defaults k to 8 and clamps it to a maximum of 30.
- IF the embedding backend is unreachable, dex returns a distinct
  `embedding-service-unreachable` status with the endpoint and a hint to fall
  back to grep/ripgrep, rather than an empty or error result.
- WHERE answer quality is gated (#550), `dex bench eval --faithfulness`
  synthesizes an `ask` answer per golden query from the retrieved evidence and
  scores how well each answer is grounded in that evidence — a model-free
  claim-overlap proxy, complementary to retrieval recall (which only scores
  whether the right files were found, not whether the synthesized prose is
  supported). It needs a chat model and supports `--check` for regression.

## Non-goals

- **Building the index.** Walking, chunking, and embedding the corpus is the
  **indexing** spec; semantic search only reads what indexing produced.
- **The storage engine & format.** How vectors, chunks, and FTS rows are stored
  and the KNN/BM25 SQL primitives are the **storage** spec; here only that a
  nearest-neighbour and a BM25 query are run.
- **The embedding model.** Which model vectorizes the query, its provider, and
  dimensions are the **embedding** spec; here only that the query is embedded.
- **Other query surfaces.** Exact identifier lookup is **symbol-search**, call/
  import topology is **graph**, and the composed one-shot answer is **ask**.
- **Result enrichment from the graph.** Role hints / centrality on hits are
  composed by **graph**/**ask**; plain semantic search returns the bare scores.
- **How search is exposed.** The MCP `find` tool wiring and the HTTP
  surface are the **mcp-server** / **http-api** specs; this spec is the behavior
  behind them.

## Checklist

- [x] Embed query → vector KNN → top-k hits with path/kind/line/snippet/cosine
- [x] Hybrid: FTS5/BM25 leg fused with semantic via RRF (k=60) when query non-empty
- [x] Wider per-leg candidate pool than final k for fusion headroom
- [x] Graph-proximity 3rd RRF lane (`DEX_GRAPH_WEIGHT`×γ^hop, default 0.6× at 1-hop) when call graph is indexed
- [x] BM25 path-column weighted 2× (`bm25(chunks_fts, 1.0, 2.0, 0.5)`)
- [x] Post-RRF local rerank: noise penalty 0.3×, definition boost 1.5×, coherence boost 1.15×, MMR decay 0.7×
- [x] Repeated identical search (≥4 in 5 min) → hint to use knowledge instead
- [x] Empty query / disabled BM25 / malformed FTS → semantic-only fallback
- [x] Cross-encoder rerank of fused pool when reranker wired and pool > k
- [x] Rerank failure (unreachable/timeout/breaker-open) → silent pre-rerank order
- [x] Rerank per-call deadline + in-process (query, id-set) cache
- [x] Consecutive-failure circuit breaker around the reranker
- [x] Per-file diversity cap; k defaults to 8, clamped to max 30
- [x] Embedding backend unreachable → distinct status + grep-fallback hint
- [x] Verified against the code by the verify workflow (flip to `living`)
