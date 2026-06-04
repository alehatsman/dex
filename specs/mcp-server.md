---
id: mcp-server
status: living
last_verified: b1e4545
owners: [aleh]
covers:
  - "internal/mcp/server.go"
  - "internal/mcp/context.go"
  - "internal/mcp/answer.go"
  - "internal/mcp/server_graph.go"
  - "internal/mcp/server_summaries.go"
  - "internal/mcp/server_noncode_map.go"
  - "internal/mcp/server_compose.go"
  - "internal/mcp/server_overview.go"
  - "internal/mcp/server_knowledge.go"
---
# MCP Server (stdio)

## Intent

`dex mcp` is dex's primary interface: a Model Context Protocol server spoken over
stdio that lets Claude Code reach the local semantic index as native tools. It
exposes a deliberately thin surface — by default a single `ask` tool that routes
a free-text question across semantic search, symbol lookup, and graph expansion
and (when a chat model is wired) synthesizes a cited answer — so an agent reaches
for one dex tool before fanning out with Grep/Glob/Read. Every tool is scoped to
one project, resolved from the caller's working directory, and every response
carries a structured `status` so that when the index is missing or a backend is
offline the agent gets an explicit fallback signal rather than a hard failure.
This spec covers the stdio MCP tool interface and its contract; the same handlers
re-exposed as REST endpoints for service clients are the http-api spec's.

## Behavior

- WHEN `dex mcp` starts, dex registers an MCP server (name `dex`) on stdio and
  blocks serving JSON-RPC until the transport closes or the context is cancelled.
- WHERE the surface is thin by default, `ask` is the only tool registered for an
  agent to see; it composes the semantic, symbol, and graph lanes and is the
  intended entry point before Grep/Glob/Read fan-out.
- WHEN a chat model is configured, `ask` returns a synthesized, citation-bearing
  (`path:line`) prose answer grounded in the evidence bundle; WHEN the chat leg
  is unreachable the answer is omitted and the caller falls back to the evidence
  bundle plus the `next_action` directive.
- WHEN `ask` runs, it infers intent
  (behavior_search/symbol_lookup/callers/callees/architecture/package_topology/editing_context),
  composes the matching lanes, and returns a compact bundle (semantic hits,
  symbols, suggested reads with inlined contents, a `next_action`, an `avoid`
  line); a caller MAY override the inferred intent.
- WHERE tool exposure is tiered, the surface is controlled by `DEX_TOOLS`
  (`ask|standard|power`; default `standard`); `DEX_EXPOSE_RAW_TOOLS=1` is a
  backward-compatible alias for `power`. `TierAsk` exposes only `ask`.
  `TierStandard` adds `overview`, `session`, `knowledge`, `file_tree`,
  `search_context`, and (when a chat model is wired) `file_view`. `TierPower` adds the full raw
  surface: `search_semantic`, `search_symbol`, `graph_neighbors`, `graph_deps`,
  `graph_callers`, `graph_callees`, `graph_links`, `graph_backlinks`,
  `graph_tags`, `graph_impact`, `graph_routes`, `code_smells`, `compress_output`, and
  `status`.
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
- IF the chat service is unreachable, `file_view` returns
  `status:"chat-service-unreachable"`; graph tools that lack indexed edges return
  `status:"no-graph"`; symbol lookup with no match returns `status:"not-found"`,
  surfacing near-miss candidates when any exist.
- WHILE the index is older than 24h, a search response is flagged `stale:true`
  with a hint to re-index, but still answers.
- WHEN `file_view` reads or summarizes a file path, the path must resolve inside
  the project root; a path escaping the root is rejected so an MCP caller can't
  read arbitrary files.
- WHILE the stdio session is live and auto-watch is enabled, the first request
  that resolves a project lazily spawns one per-project file watcher (at most once
  per project per session) that keeps the index fresh and drains pending summaries
  in the background; the watcher shares the session context and drains on shutdown.
- WHEN `file_view mode=map` is called on a non-code file (Markdown, JSON,
  YAML, TOML, lock files), dex returns a structural outline — heading tree,
  JSON key hierarchy (depth ≤ 3), YAML section hierarchy, TOML sections, or
  lock-file dependency counts — without using the index or a chat model; code
  files fall through to the existing symbol-map path.
- WHEN `search_context` is called (TierStandard), dex embeds the query, retrieves
  top-K files by aggregate chunk score (default K=3, max 5), returns per-file
  signatures and the single best-matching symbol body in one call — replacing the
  search→signatures→lines round-trip pattern.
- WHEN `file_view mode=signatures` or `mode=map` is called and the current
  session has a declared task, dex appends the body of the symbol whose qualified
  name best matches the task (BM25-style word-overlap, capped at 60 lines) so
  the reader gets task-relevant detail without a follow-up lines: call.
- WHEN `overview` is called and the chunk index is empty (indexing in progress or
  not yet started), dex returns `status:"partial"` with project markers (go.mod,
  package.json, Cargo.toml, …), a depth-2 filesystem tree, and top knowledge
  facts rather than an error, so an agent can orient without a full index.
- WHEN `file_view` is called with a `paths[]` list (max 10 paths), dex reads each
  file in the same mode and returns a concatenated result with `## path` section
  headers — collapsing a multi-file context-gathering loop into one round-trip;
  the per-path path-traversal check still applies to every entry.
- WHEN `knowledge action=add` stores a fact that was already known, the response
  shows "Confirmed (revision N)." rather than "Remembered." so a caller can tell
  whether the fact is new or repeatedly confirmed; `knowledge action=list` surfaces
  `rev N` annotations for facts with `revision_count > 1`.
- WHEN `knowledge action=consolidate` is called (requires `DEX_CHAT_URL`), dex
  reads the current session task and recent session notes, asks the chat LLM to
  extract a JSON array of factual findings, and stores each extracted fact via the
  normal `KnowledgeAdd` path — turning ad-hoc session state into durable facts
  without the caller enumerating them manually.
- WHEN `spec_verify` is called (TierPower), dex reads the spec file's `## Checklist`
  items (falling back to `## Behavior` clauses), embeds each checked `[x]` item,
  retrieves top-5 code chunks from the index, and — when a chat model is wired —
  judges each clause as `pass`/`fail`/`unknown`; unchecked `[ ]` items are returned
  as `pending` without judgment; drift is detected via `git log <last_verified>..HEAD`
  over the spec's `covers` paths and reported in `drift_commits`.

## Non-goals

- **The REST transport.** The same Server handlers re-exposed as HTTP/REST
  endpoints (`dex serve`, project-id-keyed URLs, bearer auth) are the **http-api**
  spec. This spec is the stdio MCP tool interface for Claude only.
- **What the lanes compute.** Ranking and hybrid fusion are **semantic-search**;
  exact identifier lookup is **symbol-search**; call/import/doc edges are
  **graph**; answer synthesis strategy detail is **ask**; vector storage is
  **storage**. Here only the tool surface, scoping, and status contract.
- **Building the index.** Walking, chunking, and embedding a repo are **indexing**;
  this server only reads an already-built index (and triggers re-index via watch).
- **The watch daemon internals.** Debounce, idle drain, and summary queueing are
  **watch**; here only that a session lazily spawns one and drains it on shutdown.
- **Remote MCP access.** Reaching the hot host index from a container is tracked
  separately (dex #6: stdio→REST shim now, native HTTP-MCP transport later); this
  spec is the local stdio server only.

## Checklist

- [x] `dex mcp` registers an MCP server on stdio, blocks until transport closes
- [x] `ask` is the sole TierAsk tool; composes lanes + synthesizes cited answer
- [x] 3-tier tool surface: `DEX_TOOLS=ask|standard|power`; `DEX_EXPOSE_RAW_TOOLS=1` aliases power
- [x] TierStandard: overview, session, knowledge, file_tree, search_context, file_view (chat required)
- [x] TierPower: search_semantic, search_symbol, graph_*, graph_impact, graph_routes, code_smells, compress_output, status, spec_verify
- [x] `file_view mode=map` returns structural outline for non-code files (Markdown/JSON/YAML/TOML/lock); no LLM, no index
- [x] `search_context`: single call returns top-K file signatures + best symbol body (replaces search→signatures→lines round-trip)
- [x] Task-relevance inline: signatures/map append best-matching symbol body when session has a declared task
- [x] `overview` returns `status:"partial"` with markers + depth-2 tree + knowledge facts when chunk index is empty
- [x] Read-only tools carry `readOnlyHint: true` MCP annotation
- [x] Per-project scoping: `project_root` → canonical index (cwd default)
- [x] Structured statuses: `no-index`, `embedding-service-unreachable`, `chat-service-unreachable`, `no-graph`, `not-found`, `stale`
- [x] Path traversal rejected in `file_view` (must resolve inside project root)
- [x] Lazy per-project auto-watcher spawned per session, drains on shutdown
- [x] `file_view paths[]` batch: max 10 files, same mode, concatenated `## path` output; path-traversal check per entry
- [x] `knowledge` revision tracking: `revision_count` incremented on re-add; "Confirmed (revision N)." response; `rev N` in list
- [x] `knowledge action=consolidate`: LLM-extracts facts from session notes and stores them
- [x] Remote access for containerized agents: stdio→REST shim (`dex mcp --remote`, `remote.go`) + native HTTP-MCP at `/v1/projects/{id}/mcp` (`http_mcp.go`)
- [x] Verified against the code by the verify workflow (flip to `living`)
