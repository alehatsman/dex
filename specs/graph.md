---
id: graph
status: draft
owners: [aleh]
covers:
  - "internal/graph/**"
  - "internal/mcp/server_graph.go"
  - "internal/store/store_graph.go"
  - "cmd/dex/main.go"
---
# Graph

## Intent

The graph is dex's structural map of a codebase: a persisted set of nodes
(packages, files, functions, methods, types, fields, imports — and, for docs,
markdown documents/headings/tags) and the typed edges between them. Where
semantic search answers "what code is *about* X" and symbol search answers
"where is X defined", the graph answers relationship questions — "who calls
this function", "what does it call", "what does this package import", "what's
the shape of the whole codebase", "what docs link here". These queries read
only the stored graph, so they are the cheapest tools in the surface and a
precise fallback when semantic ranking drifts. The code/call/import graph is
built from Go source and is deliberately Go-only today; the contract is that a
non-Go (or not-yet-extracted) target degrades to an empty, well-typed answer
rather than an error, so a consumer can branch on status instead of catching a
failure.

## Behavior

- WHEN a caller asks for the callers of a symbol, dex returns the functions with
  an incoming `calls` edge to it; WHEN it asks for callees, the functions it
  calls — each peer carrying its location, kind, and the call-site file:line.
- WHERE a symbol name is ambiguous, the query resolves and returns the matched
  targets (bare, receiver-qualified like `(*Server).Run`, or package-qualified),
  optionally narrowed by package, so the caller can disambiguate.
- WHEN a caller asks what a package imports, dex returns that package's import
  edges; WHEN it asks for the package map, dex returns the whole internal
  package import DAG (nodes with in/out-degree and pagerank, plus edges) in one
  call, so a "map of the codebase" consumer needs no per-package round-trips.
- WHERE the code graph is built from Go source (via go/types), a target outside
  the Go-graphed languages yields `not-found`/`no-graph` with a hint — never an
  error — so non-Go projects and consumers degrade gracefully to an empty graph.
- IF the project has an index but no persisted graph, graph queries return a
  `no-graph` status naming the reindex command, distinct from `no-index` (no
  index at all) and `not-found` (graphed, but this target/edge is absent).
- WHEN a caller queries the doc graph, dex returns the markdown links/backlinks
  of a document and tag↔document membership; these come from markdown extraction
  and are independent of the Go code graph.
- WHILE multi-language structural extraction exists as a tree-sitter layer, it
  is inert until per-language extractors are registered, so today a non-Go
  source tree contributes no code/call/import nodes.
- WHERE result counts are bounded, each query caps its hits (with per-tool
  defaults and ceilings) and orders peers deterministically.
- WHERE the same logic backs both surfaces, the `graph_*` MCP tools and the
  `dex graph …` CLI call one implementation.

## Non-goals

- **Building the graph.** Walking the tree and writing graph nodes/edges runs as
  a phase of indexing; the **indexing** spec owns producing the graph, this spec
  owns querying it (it borrows the extractor package only to define real
  behavior and `covers`).
- **The graph's on-disk tables.** The `graph_nodes`/`graph_edges` schema, chunk
  linkage, and storage engine are the **storage** spec's.
- **Symbol definition lookup.** Returning a symbol's definition by exact name is
  the **symbol-search** spec; the graph returns relationships (edges), not
  definitions (though symbol search borrows the graph's centrality for ranking).
- **Semantic neighborhood.** `graph_neighbors` returns cosine-similar chunks, not
  structural edges — it belongs to **semantic-search**, not this spec, despite
  the `graph_` name.
- **Composed retrieval.** The **ask** router fuses semantic + symbol + graph and
  decides when to expand the neighborhood; this spec is the standalone graph
  lanes it calls.
- **Non-Go code-graph extraction.** Extending the call/import graph beyond Go
  (the dormant tree-sitter layer) is future work tracked separately; here the
  contract is only that non-Go degrades to an empty, well-typed answer.

## Checklist

- [x] `graph_callers` / `graph_callees`: incoming / outgoing `calls` edges with
      call-site location; target resolution + disambiguation
- [x] `graph_deps`: a package's import edges
- [x] Whole-project package import DAG (nodes with degree/pagerank + edges) in
      one call for a codebase-map consumer
- [x] Code/call/import graph is Go-only (go/types); non-Go degrades to
      `not-found`/`no-graph`, never an error
- [x] Status taxonomy: `no-index` vs `no-graph` vs `not-found` vs `ok`, each with
      an actionable hint
- [x] Doc graph: `graph_links` / `graph_backlinks` / `graph_tags` over markdown,
      independent of the Go code graph
- [x] Tree-sitter multi-language layer present but inert until per-language
      extractors register
- [x] Per-tool result caps + deterministic ordering
- [x] Shared by `graph_*` MCP tools and `dex graph …` CLI
- [ ] Verified against the code by the verify workflow (flip to `living`)
