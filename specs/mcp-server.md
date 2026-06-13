---
id: mcp-server
status: living
last_verified: 49b3786
owners: [aleh]
covers:
  - "internal/mcp/server.go"
  - "internal/mcp/context.go"
  - "internal/mcp/answer.go"
  - "internal/mcp/server_graph.go"
  - "internal/mcp/server_noncode_map.go"
  - "internal/mcp/server_knowledge.go"
  - "internal/mcp/server_session.go"
  - "internal/mcp/server_grep.go"
  - "internal/mcp/server_tree.go"
  - "internal/mcp/server_shell.go"
  - "internal/mcp/server_smells.go"
  - "internal/mcp/server_routes.go"
  - "internal/mcp/http_mcp.go"
  - "internal/mcp/remote.go"
---
# MCP Server (stdio)

## Intent

`dex mcp` is dex's primary interface: a Model Context Protocol server spoken over
stdio that lets Claude Code reach the local semantic index as native tools. It
leads with a single `ask` tool that routes a free-text question across semantic
search, symbol lookup, and graph expansion and (when a chat model is wired)
synthesizes a cited answer — so an agent reaches for one dex tool before fanning
out with Grep/Glob/Read. Every tool is scoped to one project, resolved from the
caller's working directory, and every response carries a structured `status` so
that when the index is missing or a backend is offline the agent gets an explicit
fallback signal rather than a hard failure. This spec covers the stdio MCP tool
interface and its contract; the same handlers re-exposed as REST endpoints for
service clients are the http-api spec's.

## Behavior

- WHEN `dex mcp` starts, dex registers an MCP server (name `dex`) on stdio and
  blocks serving JSON-RPC until the transport closes or the context is cancelled.
- WHERE the tool surface is defined, a single `registerTools` path wires every
  tool onto the server. The local on-disk index (`*Server`), the remote stdio
  shim (`*remoteClient`, proxying to a `dex serve` REST endpoint), and the native
  HTTP-MCP transport all call it, so the surfaces can never drift in tool names,
  JSON schemas, or descriptions — the schema for each tool is derived by the SDK
  from one shared Input type.
- WHERE the surface is capability-derived (#283/#290) rather than tiered, tools
  are registered only when the backend they need is available:
  - The always-on lane (registered even with no embedder or chat model) is
    `ask`, `grep`, `ls`, and `shell`.
  - The graph/symbol/analysis lane (`lookup`, `deps`, `callers`, `callees`,
    `impact`, `routes`, `smells`, `path`, `diff`, `clusters`, `status`, `notes`,
    `session`) is registered whenever a non-weak model surface is in effect.
  - `find` (semantic search) is registered only WHEN a query-time embedder is
    wired (`embedAvailable`); with `DEX_EMBED_ENGINE=none` (the lean profile) it
    is omitted and retrieval degrades to BM25 (`grep`) + symbol + graph + file
    lanes — `ask` stays and routes to the non-semantic lanes.
  - `read` (chat-backed file summarization) is registered only WHEN a chat model
    is wired (`chatAvailable`).
  - WHEN a weak/local model is detected, the full surface is hidden and only the
    always-on lane (`ask`, `grep`, `ls`, `shell`) is exposed.
  This yields a flat surface of up to 19 tools, named for their purpose with no
  category prefix: `ask`, `find`, `lookup`, `deps`, `callers`, `callees`,
  `impact`, `path`, `diff`, `clusters`, `routes`, `smells`, `status`, `notes`,
  `session`, `read`, `shell`, `ls`, `grep`.
- WHEN a chat model is configured, `ask` returns a synthesized, citation-bearing
  (`path:line`) prose answer grounded in the evidence bundle; WHEN the chat leg
  is unreachable the answer is omitted and the caller falls back to the evidence
  bundle plus the `next_action` directive.
- WHEN `ask` runs, it infers intent
  (behavior_search/symbol_lookup/callers/callees/architecture/package_topology/editing_context),
  composes the matching lanes, and returns a compact bundle (semantic hits,
  symbols, suggested reads with inlined contents, a `next_action`, an `avoid`
  line); a caller MAY override the inferred intent. Inline content shares one
  per-intent byte pool across both lanes; oversize ranges arrive `truncated:true`
  with their original line range, and `no_inline:true` omits payloads.
- WHEN `ask` is called with an empty question, it routes to session-start
  orientation (intent `orient`): a deterministic, zero-inference L0+L1 codemap
  bundle (repo cluster overview plus an L1 zoom into the most-central cluster) is
  returned in the `map` field, so an agent names the right package before any
  `find`. The bundle is byte-stable across calls (cache-friendly) and is rendered
  through the same path as `dex orient` and `dex map`. WHEN no call graph is
  indexed it degrades to a `no-graph`/`no-index` hint pointing at
  `dex index . --graph=only`, never an error.
- WHEN `find` is called, dex embeds the query and returns top-k chunks; identifier
  tokens in the query are also looked up by exact symbol name and fused via RRF.
  Supports `languages`, `path_glob`, and `exclude` filters.
- WHEN `lookup` is called, dex performs a fast SQL symbol lookup (no embedding)
  and returns `not-found` when no chunk with that name exists.
- WHEN a graph tool is called (`deps`, `callers`, `callees`, `impact`, `path`,
  `diff`, `clusters`, `routes`, `smells`), dex reads the static call/import graph
  (no embedding, no chat) and returns `no-graph` when the relevant edges have not
  been indexed (`dex index . --graph=only`). `callers`/`callees`/`impact`/`path`
  accept a bare name, a qualified method (`(*Server).RunStdio`), or a
  package-qualified name and return multiple matches for disambiguation.
- WHEN `read` reads or summarizes a file path, the path must resolve inside the
  project root; a path escaping the root is rejected so an MCP caller can't read
  arbitrary files. Files over 64 KB are truncated. `paths[]` (max 10) reads
  several files in one call under the same mode with `## path` section headers,
  and the per-path traversal check applies to every entry. Responses carry an
  `etag` (content hash): on re-read pass it back to get `status:unchanged` (reuse
  context) or `status:delta` (a compact unified diff). `mode=skeleton` emits
  exported type declarations in full plus signatures with `@B<n>` body handles
  expandable on demand. Passing `task` routes compression by intent.
- WHEN `read mode=map` is called on a non-code file (Markdown, JSON, YAML, TOML,
  lock files), dex returns a structural outline — heading tree, JSON key
  hierarchy (depth ≤ 3), YAML/TOML sections, or lock-file dependency counts —
  without using the index or a chat model; code files fall through to the symbol-
  map path.
- WHEN `notes` is called, dex manages persistent project knowledge that survives
  session resets: `action=add` (store a fact with an archetype and confidence),
  `action=list` (top-k by salience), `action=delete` (by id). Archetypes:
  Architecture | Gotcha | Convention | Decision | Observation | Dependency |
  Pattern | Fact. High-salience facts are injected into `ask` responses as
  `knowledge_facts`. No embedding required.
- WHEN `session` is called, dex manages per-project session memory across tool
  calls: `set_task`, `add_note`, `add_file`, `get`, `clear`, `snapshot` (recovery
  block after compaction), `budget` (context-window utilization estimate),
  `heatmap` (per-file access frequency and compression savings). The task + notes
  + files are surfaced in `ask` responses as `session_task`. No embedding required.
- WHEN `shell` is called, dex executes a command and returns compressed output
  (the same pipeline as the indexer's log compression). File-write redirects
  (`>`, `>>`) and `tee` are blocked — the caller must use the Write tool;
  `raw:true` skips compression; timeout 60 s.
- WHEN `grep` is called, dex runs an RE2 pattern search over indexed project files
  (inheriting the project's ignore rules), falling back to a filesystem walk that
  skips `.git`, `vendor`, and `node_modules` when no index exists. Returns up to
  `max_results` (default 50) matches and `status:"no-matches"` when nothing hits.
- WHEN `ls` is called, dex lists indexed files under a directory, showing
  individual files within `depth` levels (default 3) and aggregating deeper files
  into their parent dirs with summed chunk counts. No embedding required.
- WHERE MCP annotations are set, all read-only tools carry `readOnlyHint: true`
  so hosts (e.g. Claude Code plan mode) can skip approval prompts for them.
- WHEN any tool resolves a project, it canonicalizes the caller's `project_root`
  (defaulting to the server's working directory) to a single per-project index,
  so every tool reads the index for exactly the repo the agent is working in.
- IF no `project_root` is given and the working directory can't be determined, a
  tool returns `status:"error"` with a hint to pass `project_root` explicitly.
- IF the resolved project has no index on disk, a tool returns `status:"no-index"`
  with a hint to run `dex index <root>` and fall back to grep meanwhile.
- IF the embedding service is unreachable, a search/ask tool returns
  `status:"embedding-service-unreachable"` with the endpoint and a hint to fall
  back to grep/Glob/ripgrep — never a partial or empty result masquerading as ok.
- IF the chat service is unreachable, `read` returns
  `status:"chat-service-unreachable"`; graph tools that lack indexed edges return
  `status:"no-graph"`; symbol lookup with no match returns `status:"not-found"`,
  surfacing near-miss candidates when any exist.
- WHILE the index is older than 24h, a search response is flagged `stale:true`
  with a hint to re-index, but still answers.
- WHILE the stdio session is live and auto-watch is enabled, the first request
  that resolves a project lazily spawns one per-project file watcher (at most once
  per project per session) that keeps both the chunk and graph lanes fresh on
  change (its `AfterIndex` hook re-runs the graph phase and embeds new nodes); the
  watcher shares the session context and stops on shutdown.
- WHERE a handler panics, an `addTool` recovery guard converts it to a structured
  tool error instead of crashing the MCP session.

## Non-goals

- **The REST transport.** The same handlers re-exposed as HTTP/REST endpoints
  (`dex serve`, project-id-keyed URLs, bearer auth) are the **http-api** spec.
  This spec is the stdio MCP tool interface.
- **What the lanes compute.** Ranking and hybrid fusion are **semantic-search**;
  exact identifier lookup is **symbol-search**; call/import edges are **graph**;
  answer synthesis strategy detail is **ask**; vector storage is **storage**.
  Here only the tool surface, scoping, and status contract.
- **Building the index.** Walking, chunking, and embedding a repo are **indexing**;
  this server only reads an already-built index (and triggers re-index via watch).
- **The watch daemon internals.** Debounce and the `AfterIndex` graph refresh are
  **watch**; here only that a session lazily spawns one and stops it on shutdown.
- **Remote MCP access.** Reaching the hot host index from a container goes through
  the stdio→REST shim (`dex mcp --remote`, `remote.go`) and the native HTTP-MCP
  transport at `/v1/projects/{id}/mcp` (`http_mcp.go`); both register the same
  surface via `registerTools`, so the tool contract here applies unchanged.

## Checklist

- [x] `dex mcp` registers an MCP server on stdio, blocks until transport closes
- [x] `ask` leads the surface; composes lanes + synthesizes cited answer when a chat model is wired
- [x] `ask` with an empty question → session-start orientation (intent `orient`): deterministic L0+L1 codemap in `map`, byte-stable, single-sourced with `dex orient`/`dex map`; degrades to a `no-graph`/`no-index` hint, never an error (#348 / #316 story 6)
- [x] Single `registerTools` path wires the surface for stdio, remote shim, and HTTP-MCP — no name/schema drift
- [x] Capability-derived exposure (#283/#290): `find` gated on `embedAvailable`, `read` on `chatAvailable`; weak model → only `ask`/`grep`/`ls`/`shell`
- [x] Flat, prefix-free surface of up to 19 tools (no `DEX_TOOLS` tiers)
- [x] `read mode=map` returns a structural outline for non-code files (Markdown/JSON/YAML/TOML/lock); no LLM, no index
- [x] `read paths[]` batch: max 10 files, same mode, concatenated `## path` output; path-traversal check per entry; etag/delta/skeleton modes
- [x] `read` path traversal rejected (must resolve inside project root); files over 64 KB truncated
- [x] `notes` actions add/list/delete; high-salience facts injected into `ask` as `knowledge_facts`
- [x] `session` actions set_task/add_note/add_file/get/clear/snapshot/budget/heatmap; surfaced in `ask` as `session_task`
- [x] `shell` blocks `>`/`>>`/`tee`; `raw:true` skip; 60 s timeout; compressed output
- [x] `grep` RE2 search over indexed files; fs-walk fallback; `max_results` cap (default 50); `no-matches` status
- [x] Read-only tools carry `readOnlyHint: true` MCP annotation
- [x] Per-project scoping: `project_root` → canonical index (cwd default)
- [x] Structured statuses: `no-index`, `embedding-service-unreachable`, `chat-service-unreachable`, `no-graph`, `not-found`, `stale`
- [x] Lazy per-project auto-watcher spawned per session, refreshes chunk + graph lanes, stops on shutdown
- [x] Handler panics converted to structured tool errors by the `addTool` recovery guard
- [x] Verified against the code by the verify workflow (flip to `living`)
