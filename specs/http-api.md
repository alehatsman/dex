---
id: http-api
status: living
last_verified: 3a975eb
owners: [aleh]
covers:
  - "internal/mcp/http.go"
  - "internal/mcp/server_summaries.go"
  - "cmd/dex/serve.go"
---
# HTTP API (dex serve)

## Intent

`dex serve` exposes dex's query surface as a small versioned REST API over the
network, so external service clients — notably the moongit server's dex client
and containerized coding agents — can reuse one hot, host-side index instead of
each re-indexing. It runs the same `Server` and the same tool handlers as the
stdio MCP server; only the wire protocol differs (JSON over HTTP under `/v1`
instead of MCP JSON-RPC over stdio). The daemon serves a fixed, operator-supplied
set of projects keyed by a stable project id (the hash of the repo's real path),
gated by a bearer token, and refuses to expose itself unauthenticated on a
non-loopback address. This spec covers the REST endpoint contract; the stdio MCP
tool interface for Claude is the mcp-server spec's.

## Behavior

- WHEN `dex serve` starts, dex binds the configured address and serves a JSON
  REST API under `/v1`, reusing the same Server handlers as the stdio MCP path;
  binding errors surface synchronously rather than from a background goroutine.
- WHERE the operator supplies the project set, each `--project <root>` is resolved
  to a stable id (sha256 of the symlink-resolved absolute path); the daemon serves
  only those ids and does not auto-discover. Missing-index projects start anyway
  and return `no-index` per call, with a startup warning.
- WHEN a request targets a project, the URL carries the project id
  (`/v1/projects/{id}/...`); an unknown id returns 404 and a missing id 400. The
  request body's project field is always overridden with the registry's root, so
  a client cannot smuggle a different path via the body.
- WHERE auth is required, every authenticated route demands
  `Authorization: Bearer <token>` when a token is configured (`DEX_SERVE_TOKEN`),
  compared in constant time; a missing or mismatched token returns 401.
- IF no token is configured AND the bind address is non-loopback (including the
  bare `:port` all-interfaces form), `dex serve` refuses to start — an
  unauthenticated public listener is rejected at startup.
- WHERE liveness is unauthenticated, `GET /v1/healthz` and `GET /v1/version`
  answer without a token; `GET /v1/nav` (tool-routing guide, not project-scoped) is
  also unauthenticated; all other routes are authenticated.
- WHEN a client lists the served projects, `GET /v1/projects` returns each id with
  its root; `GET /v1/status` returns global endpoint health plus per-project index
  stats (chunk/file counts, last-indexed, pending summaries).
- WHEN a client queries a project, the per-project routes mirror the dex tools:
  `POST .../ask`, `POST .../search/{semantic,symbol,context,grep}`,
  `POST .../graph/{neighbors,deps,callers,callees,links,backlinks,tags,impact,routes}`,
  `GET .../graph/packages`, `GET .../summaries`, `POST .../file/{view,tree}`,
  `POST .../code/smells`, `POST .../spec/verify`, `POST .../view/overview`,
  `POST .../knowledge`, `POST .../session`, and `POST .../shell`.
  `GET /v1/nav` is a global (non-project-scoped) route that returns the dex
  tool-routing guide for the active server configuration.
- WHEN a handler returns a tool result, the same structured `status` the stdio
  tools use (`ok`/`no-index`/`embedding-service-unreachable`/`no-graph`/
  `not-found`/`error`) is carried in the JSON body with HTTP 200; a malformed
  request body is 400, an internal failure 500, and a panic is recovered to a
  generic 500 without leaking internals.
- WHILE serving, each request is access-logged and the request body is size-capped;
  on context cancellation the server drains in-flight requests within a bounded
  shutdown budget.
- WHILE serving with eager-watch enabled, the daemon spawns one file watcher per
  registered project at startup so `dex serve` is the single re-index watcher on
  the host box; this is idempotent with the lazy on-query watcher.

## Non-goals

- **The stdio MCP tool interface.** The same handlers exposed as MCP tools over
  stdio for Claude Code (the `ask`-default surface, `DEX_EXPOSE_RAW_TOOLS`, the
  lazy per-session watcher) are the **mcp-server** spec. This spec is the REST
  endpoint contract for service clients only.
- **Native HTTP-MCP transport.** Speaking MCP JSON-RPC over HTTP — so `claude`
  attaches this daemon as an MCP server directly — now ships alongside the REST
  surface as the streamable-HTTP handler mounted at `/v1/projects/{id}/mcp`
  (dex #49; `internal/mcp/http_mcp.go`), behind the same bearer auth and project
  scoping. It is a sibling transport, not part of this REST contract; this spec
  remains the plain JSON-over-HTTP shape.
- **What each tool computes.** Ranking/fusion is **semantic-search**, identifier
  lookup is **symbol-search**, edges are **graph**, synthesis is **ask**, vector
  storage is **storage** — the handlers delegate to those. Here only the wire
  contract: routing, auth, scoping, status/HTTP-code mapping.
- **Building/refreshing the index.** Indexing is **indexing** and the watcher is
  **watch**; this spec only reads an existing index and (optionally) spawns the
  watcher.
- **Operator deployment.** systemd units, TLS termination, and how the moongit box
  runs `dex serve` are provisioning, not this spec.

## Checklist

- [x] `dex serve` binds + serves a versioned `/v1` JSON REST API (shared handlers)
- [x] Project registry from `--project` roots, keyed by sha256(realpath) id; no auto-discovery
- [x] `{id}` URL scoping; unknown id 404, body project field overridden server-side
- [x] Bearer auth (`DEX_SERVE_TOKEN`), constant-time; 401 on missing/bad token
- [x] No-token + non-loopback bind refused at startup
- [x] Unauthenticated `healthz`/`version`/`GET /v1/nav`; authed projects/status/per-project tool routes
- [x] Per-project routes mirror all MCP tools (ask/search/{semantic,symbol,context,grep}/graph/*/file/*/code/smells/spec/verify/view/overview/knowledge/session/shell); structured status in body
- [x] `GET /v1/nav` global route — tool-routing guide for the active tier; no project scoping; unauthenticated
- [x] Body size cap, access log, panic→500, bounded graceful shutdown
- [x] Eager per-project watcher at startup (idempotent with lazy path)
- [x] Native HTTP-MCP transport for direct `claude` attach — streamable handler at `/v1/projects/{id}/mcp` (dex #49)
- [x] Verified against the code by the verify workflow (flip to `living`)
