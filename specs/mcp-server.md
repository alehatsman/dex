---
id: mcp-server
status: living
last_verified: 75acce8
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
  - "internal/mcp/server_agent.go"
  - "internal/mcp/server_nav.go"
  - "internal/mcp/server_grep.go"
  - "internal/mcp/server_prefetch.go"
  - "internal/mcp/server_workspace.go"
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
  `TierStandard` adds `ctx_nav`, `ctx_overview`, `ctx_session`, `ctx_knowledge`,
  `ctx_agent`, `ctx_feedback`, `ctx_shell`, `ctx_prefetch`, `file_tree`,
  `search_grep`, `search_context`, `search_workspace`, and (when a chat model is
  wired) `file_view`. `TierPower` adds the full raw surface: `search_semantic`,
  `search_symbol`, `search_similar`, `graph_neighbors`, `graph_deps`,
  `graph_callers`, `graph_callees`, `graph_links`, `graph_backlinks`, `graph_tags`,
  `graph_impact`, `graph_routes`, `graph_smells`, `compress_output`, `status`,
  and `spec_check`.
- WHERE a tool is named, it follows the **naming convention**: a category
  prefix groups related tools so an agent can guess a name from its purpose —
  `search_*` (retrieval lanes: `search_semantic`, `search_symbol`, `search_context`,
  `search_grep`), `graph_*` (static-graph queries, incl. `graph_smells`),
  `file_*` (file access: `file_view`, `file_tree`), `ctx_*` (cross-cutting agent
  context: `ctx_nav`, `ctx_overview`, `ctx_session`, `ctx_knowledge`, `ctx_agent`,
  `ctx_shell`), `spec_*` (spec verification). The sole prefix-free names are
  the primary entry verb `ask` and the meta-tools `status` / `compress_output`.
  REST routes keep resource-noun paths (`/session`, `/view/overview`) — the
  `ctx_` prefix is an MCP tool-discovery convention, not a REST one.
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
- WHEN `ctx_overview` is called and the chunk index is empty (indexing in progress or
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
- WHEN `spec_check` is called (TierPower), dex reads the spec file's `## Checklist`
  items (falling back to `## Behavior` clauses), embeds each checked `[x]` item,
  retrieves top-5 code chunks from the index, and — when a chat model is wired —
  judges each clause as `pass`/`fail`/`unknown`; unchecked `[ ]` items are returned
  as `pending` without judgment; drift is detected via `git log <last_verified>..HEAD`
  over the spec's `covers` paths and reported in `drift_commits`.
- WHEN `ctx_agent` is called (TierStandard), it manages a per-project multi-agent
  coordination bus backed by `agents` and `agent_messages` SQLite tables.
  `action=announce` registers or refreshes an agent by `agent_id` and `role` (upsert).
  `action=post` appends a message with an optional `topic` and required `body`,
  bumps the poster's `last_seen_at`, and returns the new `message_id`.
  `action=read` returns messages in insertion order filtered by optional `topic` and
  paginated via `since_id` (exclusive lower bound on message id); `limit` defaults to
  50 (max 200).
  `action=list` returns all registered agents ordered by `last_seen_at` descending.
  The bus is useful in orchestration scenarios where multiple concurrent agents
  query the same dex instance and need to share findings or avoid duplicate work.
- WHEN `ctx_nav` is called (TierStandard), dex returns a structured tool-routing
  guide listing every tool available at the active tier — its name, tier membership
  (`all`/`standard`/`power`), one-line purpose, and a when-to-call guidance line.
  The response also includes a `guide` field: a concise markdown routing summary
  oriented toward the active tier's tool surface. `ctx_nav` requires no index,
  no embedding, and no chat model; it is a zero-argument orientation call intended
  to be made once at session start in an unfamiliar project. REST parity at
  `GET /v1/nav` (global, not project-scoped).
- WHEN `ctx_shell` is called (TierStandard), dex executes a shell command and returns
  compressed output. The output policy is three-tier: `passthrough` for auth flows,
  interactive REPLs, and dev servers (output unchanged, auth device-codes preserved);
  `verbatim` for curl, jq, cat, git log (ANSI stripped, structure preserved);
  `compress` for build/test/lint runs (56+ patterns, 60–99% line reduction). File-write
  redirects (`>`, `>>`) and `tee` are blocked; the caller must use the Write tool.
  `raw:true` skips compression. Exits and returns `exit_code`, `lines_saved`, and
  `output`. Timeout: 60 s.
- WHEN `search_grep` is called (TierStandard), dex performs a literal or RE2-regex
  pattern search across all indexed project files. The search uses the index's file
  list when available (inheriting the project's ignore rules); when the index is
  absent it falls back to a filesystem walk skipping `.git`, `vendor`, and
  `node_modules`. Returns up to `max_results` matches (default 50) with path, line
  number, and trimmed content. Returns `status:"no-matches"` when nothing matches.
  `search_grep` complements `ask`/`search_semantic` for exact-match queries —
  cross-cutting symbol references, import paths, string literals — that semantic
  search misses.
- WHEN `ctx_prefetch` is called (TierStandard), dex accepts `changed_files[]` and
  uses spreading activation over the call/import graph to find the most
  structurally-related neighbor files. Each neighbor is read at a fidelity
  determined by `budget_tokens`: ≥80% remaining → full summary (LLM); ≥40% → map
  (imports+symbols); else → signatures. Without `budget_tokens` all files use
  signatures (fast, no LLM). Pass `task` to boost files whose paths match task
  keywords. Returns `files[]` with content inlined — no follow-up reads needed.
  Returns `no-index` when no graph is available; requires a graph index
  (`dex index . --graph`).
- WHEN `search_workspace` is called (TierStandard), dex reads `.dex/workspace.yml`
  from the project root, embeds the query once, runs `Store.Search` per listed
  project, and merges results with RRF (k=60). Each hit is tagged
  `[project:label]` for attribution, where `label` defaults to the directory name
  if not specified in the YAML. Returns `status:"no-workspace"` when no
  `workspace.yml` is found; `status:"no-index"` when none of the listed projects
  is indexed. Requires the embedding service and each project to be indexed
  independently.
- WHEN `ctx_feedback` is called (TierStandard), dex records output-ratio feedback
  to the adaptive compression policy: the caller passes `intent`
  (read/search/refactor/generate/test/debug/review), the ratio of output tokens to
  context tokens, and the `file_view` mode used in the last read this turn. dex
  uses these signals to downgrade compression modes that consistently produce thin
  responses. Skip when no `file_view` was called this turn.

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
- [x] TierStandard: ctx_nav, ctx_overview, ctx_session, ctx_knowledge, ctx_agent, ctx_feedback, ctx_shell, ctx_prefetch, file_tree, search_grep, search_context, search_workspace, file_view (chat required)
- [x] TierPower: search_semantic, search_symbol, search_similar, graph_*, graph_impact, graph_routes, graph_smells, compress_output, status, spec_check
- [x] `file_view mode=map` returns structural outline for non-code files (Markdown/JSON/YAML/TOML/lock); no LLM, no index
- [x] `search_context`: single call returns top-K file signatures + best symbol body (replaces search→signatures→lines round-trip)
- [x] Task-relevance inline: signatures/map append best-matching symbol body when session has a declared task
- [x] `ctx_overview` returns `status:"partial"` with markers + depth-2 tree + knowledge facts when chunk index is empty
- [x] Read-only tools carry `readOnlyHint: true` MCP annotation
- [x] Per-project scoping: `project_root` → canonical index (cwd default)
- [x] Structured statuses: `no-index`, `embedding-service-unreachable`, `chat-service-unreachable`, `no-graph`, `not-found`, `stale`
- [x] Path traversal rejected in `file_view` (must resolve inside project root)
- [x] Lazy per-project auto-watcher spawned per session, drains on shutdown
- [x] `file_view paths[]` batch: max 10 files, same mode, concatenated `## path` output; path-traversal check per entry
- [x] `ctx_knowledge` revision tracking: `revision_count` incremented on re-add; "Confirmed (revision N)." response; `rev N` in list
- [x] `knowledge action=consolidate`: LLM-extracts facts from session notes and stores them
- [x] `ctx_agent` coordination bus (TierStandard): `announce`/`post`/`read`/`list` actions; topic filtering; `since_id` pagination; REST at `/v1/projects/{id}/agent`
- [x] `ctx_nav` (TierStandard): returns structured tool catalogue + markdown routing guide; zero-arg, no index/embed required; REST at `GET /v1/nav`
- [x] `ctx_shell` (TierStandard): 3-tier output policy (passthrough/verbatim/compress); auth-flow detection; heredoc/redirect block; `raw:true`; 60 s timeout; REST at `POST /v1/shell`
- [x] `search_grep` (TierStandard): RE2 pattern search over indexed files; index file-list when available, fs-walk fallback; `max_results` cap (default 50); `no-matches` status
- [x] `ctx_prefetch` (TierStandard): spreading-activation blast-radius over changed_files[]; fidelity auto-selected by budget_tokens; content inlined; `no-index` when no graph
- [x] `search_workspace` (TierStandard): multi-project RRF merge from .dex/workspace.yml; embed once, search per project, tag hits [project:label]; `no-workspace`/`no-index` statuses
- [x] `ctx_feedback` (TierStandard): output-ratio feedback to adaptive compression policy; intent + ratio + last read mode; skip when no file_view called
- [x] `search_similar` (TierPower): hybrid pipeline over code at file:line anchor; stronger than graph_neighbors; anchor excluded; supports languages/path_glob/exclude filters
- [x] Tool naming category-prefix convention: `search_*`, `graph_*`, `file_*`, `ctx_*`, `spec_*`; `ask`/`status`/`compress_output` are prefix-free
- [x] Remote access for containerized agents: stdio→REST shim (`dex mcp --remote`, `remote.go`) + native HTTP-MCP at `/v1/projects/{id}/mcp` (`http_mcp.go`)
- [x] Verified against the code by the verify workflow (flip to `living`)
