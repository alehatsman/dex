---
id: symbol-search
status: draft
owners: [aleh]
covers:
  - "internal/store/store.go"
  - "internal/mcp/server.go"
  - "cmd/dex/main.go"
---
# Symbol Search

## Intent

Symbol search answers "where is the symbol named X defined?" — an exact,
structural lookup of a code symbol (function, method, type, class, …) by its
identifier, returning the definition location(s), kind, and signature/body. It
is the cheap, deterministic counterpart to semantic search: it reads only the
structural metadata captured at index time, so it needs no embedding backend and
no vector math. An agent that already knows a name reaches for symbol search to
jump straight to the definition instead of paying for an embedding round-trip or
falling back to grep. Results are ranked by graph centrality so the same name
resolves to the same top hit on every call, and a near-miss surface turns a
typo into an actionable retry rather than a dead end.

## Behavior

- WHEN a caller looks up an exact identifier, dex returns every indexed chunk
  whose recorded name matches exactly (case-sensitive), each with its path,
  line range, kind, and content.
- WHERE the lookup is purely structural, it reads the index's stored metadata
  and never contacts the embedding backend, so symbol search works even when the
  embedding model is unreachable.
- WHEN multiple chunks share a name, results are ordered by graph centrality
  (pagerank, then in-degree) with a path/line tiebreak, so the most-referenced
  definition ranks first and the ordering is stable across runs.
- WHEN a name matches no chunk, dex falls back to the Go relationship-graph nodes
  and returns any node whose name matches exactly with a known file location —
  surfacing types, fields, and entities that don't produce standalone chunks.
- IF the name still matches nothing, dex reports a not-found status and offers a
  "did you mean" list of distinct indexed names containing the query as a
  substring (shortest first), so the caller can retry with a real identifier.
- WHEN the lookup name is empty, dex reports an error rather than scanning.
- WHEN no index exists for the target project, dex reports a no-index status that
  names the `dex index` command to run, rather than failing opaquely.
- WHILE a result count cap applies, the caller may set it (default 10); both the
  exact-match query and the graph fallback honor it.
- WHERE the same logic backs both surfaces, the `search_symbol` MCP tool and the
  `dex search symbol [<path>] <name>` CLI call one implementation, and the CLI
  can emit text or JSON.

## Non-goals

- **Symbol extraction.** Parsing source into named, kinded chunks (tree-sitter
  per language, the `name`/`kind` columns) happens at index time and is the
  **indexing** spec's; symbol search only queries what was stored.
- **The on-disk index.** The `chunks`/`graph_nodes` tables, their columns, and
  the storage engine are the **storage** spec's.
- **Meaning-based / fuzzy retrieval.** Ranking chunks by semantic similarity to a
  natural-language query is the **semantic-search** spec; symbol search is exact
  by name (the substring "did you mean" list is a hint, not a ranked result set).
- **Relationship queries.** Callers/callees and the import/package map of a
  symbol are the **graph** spec; symbol search returns definitions, not edges
  (it only borrows centrality columns for ordering).
- **Composed retrieval.** The **ask** router fuses semantic + symbol + graph into
  one response; this spec is the standalone symbol lane it can call.

## Checklist

- [x] Exact (case-sensitive) name match over indexed chunks, returning
      path/kind/line-range/content
- [x] Structural only — no embedding backend required
- [x] Ranked by graph centrality (pagerank, in-degree) with path/line tiebreak;
      stable ordering
- [x] Go-graph-node fallback when no chunk matches (types/fields without chunks)
- [x] Not-found returns a "did you mean" substring candidate list
- [x] Empty-name → error; missing index → no-index status naming `dex index`
- [x] Caller-settable result cap (default 10), honored by match + fallback
- [x] Shared by `search_symbol` MCP tool and `dex search symbol` CLI (text/JSON)
- [ ] Verified against the code by the verify workflow (flip to `living`)
