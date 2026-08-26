# 95i — selector-grammar seeds (Phase 3, static)

Status: **building** · Tracking: #210 · Builds on: #206 pipes (95h), spec `specs/query-pipe.md` §Phasing

## What this delivers

A new **seed** shape for `query`: structured selectors that enumerate a symbol
set straight from the graph index, instead of shape-inference. They compose with
pipes like any other seed — this is the first Phase 3 item, and it is **100%
deterministic** (graph-index SQL, no model), per the roadmap's static-first rule.

```
query("pkg:store | callers | impact")   # blast radius of everything in a package
query("func:*Handler | callees")         # what every *Handler calls
query("type:*Output")                    # standalone: list matching symbols
query("pkg:mcp func:*Handler")           # AND: *Handler funcs in the mcp package
```

## Grammar

```
selector-query := selector ( ws selector )*
selector       := field ":" pattern
field          := pkg | func | type | file | kind
```

- A seed segment is a **selector query** iff *every* whitespace-separated token
  matches `^(pkg|func|type|file|kind):`. This disambiguates cleanly from
  `server.go:829` (head is not a field keyword) and from prose (tokens have no
  `field:` shape). Mixed input (`pkg:store foo`) is NOT a selector — it falls
  through to prose, honestly.
- Multiple selectors **AND** together.
- Field semantics (grounded in the `graph_nodes` schema — indexed columns
  `name`, `package_path`, `file_path`, `kind`, ranked by `pagerank`):

  | field | column | match |
  |---|---|---|
  | `func:` | `name` + kind ∈ {function,method} | glob (bare = exact) |
  | `type:` | `name` + kind ∈ {struct,interface,type} | glob (bare = exact) |
  | `pkg:`  | `package_path` | glob (bare = substring) |
  | `file:` | `file_path` | glob (bare = substring) |
  | `kind:` | `kind` | exact (combines as an extra filter) |

- Glob: `*` → any run, `?` → one char; translated to SQL `LIKE` (`%`/`_`) with
  existing `%`/`_`/`\` escaped, `ESCAPE '\'` (the pattern already used by
  `ExportedSymbolsByDir`). Name fields anchor the LIKE (exact when no wildcard);
  path fields wrap bare terms in `%…%` (paths are long — substring is ergonomic).
- Results ordered by `pagerank DESC` (most central first), capped at `k` (default
  `selectDefaultK`, reuses the query `k` when the caller sets it).

## Integration points (validated against current code)

- **Classifier:** `classifyQuery` (`internal/mcp/query.go`) gains a selector check
  at the very top (before `pathLineRange`/`classifyLookTarget`), returning a new
  `laneRoute{lane:"select"}` and detected shape `"selector"`. Everything else is
  unchanged — a non-selector input never reaches the new branch's positive case.
- **Dispatch:** `dispatchSelector` (new, `query_select.go`) parses the tokens into
  a `selectorSpec`, calls the store, and builds a `QueryOutput` whose **`Refs`**
  carry the selected symbols (the currency pipes thread) and whose `Result.Select`
  is a compact `SelectResult{Count, Symbols []Ref}` for standalone readability.
  `route.lane = "select"`, `trust.provenance = "name-based"` (tree-sitter symbol
  table, not a resolved edge).
- **Pipe seed:** works for free — the pipe seed goes through `dispatchSingle`
  (`query_pipe.go`), which routes a selector exactly like any other seed, so
  `pkg:store | callers` needs no pipe-specific code.
- **Store:** `SelectSymbols(ctx, spec, limit)` (`store_graph.go`) — one indexed
  SQL query, `WHERE` built from the non-empty spec fields. Returns `[]GraphSymbol`
  (existing type).
- **Capability:** the store call rides an optional `symbolSelectorSource`
  interface on `*Server`, type-asserted like `symbolCoercer`/`seenLooker` — not a
  new `toolSurface` method (remote runs the whole query server-side on a
  `*Server`). Absent capability → honest error.

## Wire (additive, no breakage)

- `QueryResult` gains `Select *SelectResult` (`omitempty`) — a new lane pointer,
  exactly one populated as before.
- `SelectResult{Count int, Symbols []Ref}` reuses the wire `Ref` type — no new
  symbol type. `route.lane` adds `select`; `route.detected` adds `selector`.
- `tool_schema_contract_test` pins tool INPUTS only (#207), so an output-union
  addition needs no golden regen. `QueryInput` is unchanged (selectors ride the
  existing `input` string), so no input-contract change either.

## MVP scope / deferred

**In:** the five node-attribute selectors, AND-composition, glob→LIKE, pagerank
order + cap, standalone + pipe-seed use, `route` echo.

**Deferred** (grow from use): `calls:<sym>` and other edge selectors (an edge
query — expressible today as `sym | callers`), OR-composition / unions, negation,
and — per the roadmap — all LLM-backed transforms (dead last).

## Validation

1. Parser unit test (token → `selectorSpec`; selector-vs-prose-vs-path disambig).
2. Store test: `SelectSymbols` filters + ordering on a seeded fixture.
3. E2E: `func:pipe*` selects the fixture funcs; `pkg:… | callers` composes as a
   pipe seed and matches the manual two-step.
4. `mooncake task ci` green.
