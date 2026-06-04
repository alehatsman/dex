---
id: graph
status: living
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
multi-language: Go is extracted with full type resolution (`go/types`), while
Python, JavaScript, TypeScript, Rust, and Java are extracted with name-based
tree-sitter parsers. Go edges are type-resolved and precise; tree-sitter edges
are name-based and stamped `metadata.provenance = "sitter"` so consumers can
tell them apart. A language with no registered extractor (or a target dex
hasn't graphed) degrades to an empty, well-typed answer rather than an error,
so a consumer can branch on status instead of catching a failure.

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
- WHERE the code graph spans Go (via go/types) plus the tree-sitter languages
  (Python, JS, TS, Rust, Java), a target in an un-extracted language yields
  `not-found`/`no-graph` with a hint — never an error — so projects in other
  languages and their consumers degrade gracefully to an empty graph.
- IF the project has an index but no persisted graph, graph queries return a
  `no-graph` status naming the reindex command, distinct from `no-index` (no
  index at all) and `not-found` (graphed, but this target/edge is absent).
- WHEN a caller queries the doc graph, dex returns the markdown links/backlinks
  of a document and tag↔document membership; these come from markdown extraction
  and are independent of the Go code graph.
- WHILE multi-language structural extraction runs as a tree-sitter layer, each
  per-language extractor registers itself (Python, JS, TS, Rust, Java) and
  contributes package/file/function/method/class/interface/import nodes plus
  contains/calls/has_method/imports edges; nodes carry `metadata.language` and
  a source tree in an unregistered language contributes no nodes.
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
- **Building the multi-language graph.** Defining each tree-sitter extractor's
  grammar, node/edge emission, and cross-file name resolution belongs to the
  **indexing** spec and the extractor source; here the contract is only that
  the resulting graph is queryable and that un-extracted languages degrade to an
  empty, well-typed answer. Type-resolved precision for the tree-sitter
  languages (the LSP-as-consumer upgrade) remains future work.

## Checklist

- [x] `graph_callers` / `graph_callees`: incoming / outgoing `calls` edges with
      call-site location; target resolution + disambiguation
- [x] `graph_deps`: a package's import edges
- [x] Whole-project package import DAG (nodes with degree/pagerank + edges) in
      one call for a codebase-map consumer
- [x] Code/call/import graph spans Go (go/types, type-resolved) plus tree-sitter
      languages (Python, JS, TS, Rust, Java, name-based); un-extracted languages
      degrade to `not-found`/`no-graph`, never an error
- [x] Status taxonomy: `no-index` vs `no-graph` vs `not-found` vs `ok`, each with
      an actionable hint
- [x] Doc graph: `graph_links` / `graph_backlinks` / `graph_tags` over markdown,
      independent of the Go code graph
- [x] Tree-sitter multi-language layer live: per-language extractors registered
      for Python, JS, TS, Rust, Java; edges stamped `provenance=sitter`, nodes
      carry `metadata.language`
- [x] Per-tool result caps + deterministic ordering
- [x] Shared by `graph_*` MCP tools and `dex graph …` CLI
- [x] Verified against the code by the verify workflow (flip to `living`)
