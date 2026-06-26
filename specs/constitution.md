---
id: constitution
status: living
owners: [aleh]
---
# Constitution

## Intent

dex is a local semantic-search server for Claude Code: it indexes a repository
and serves `ask`, `find`, `map`, and the graph/analysis tools (`trace` with
`--dir callers|callees|path|impact`, `deps`, `clusters`, …) over MCP so an agent can find code by meaning, not just by string. This spec is
the project's constitution — the repo-wide invariants every subsystem inherits.
It does not describe any one surface (those are the indexing, storage,
semantic-search, symbol-search, graph, ask, embedding, mcp-server, http-api,
watch, and ignore specs); it states the principles those specs must not
violate. It is `living`: these are commitments held true today, not a backlog.

## Behavior

- WHILE running, dex operates entirely on the developer's own machine — it
  indexes local repositories and talks to a local embedding/chat backend, with no
  dependency on a cloud service or remote multi-tenant store.
- WHERE dex ships, it is a single Go binary CLI; subcommands (`index`, `watch`,
  `serve`, …) are facets of one tool, not a fleet of services.
- WHILE serving an agent, MCP is the primary interface: the tools Claude Code
  calls are the product surface, and other entry points exist to support that use.
- WHERE the index is built or read, the binary must be compiled with CGO and the
  `sqlite_fts5` build tag — this requirement is load-bearing, because a binary
  without it cannot open the index and a destructive reindex from such a binary
  would wipe the index rather than rebuild it.
- WHERE a repository is indexed, indexing is opt-in: dex never embeds a tree the
  owner did not explicitly include, so pointing dex at a machine indexes nothing
  until a project declares its include set.
- IF the embedding backend is unreachable, dex degrades rather than breaks — it
  surfaces a distinct unreachable condition so a consumer can fall back to grep
  instead of trusting a silently empty or partial index.
- WHILE a project is being worked on, the index is kept current by the watch
  daemon so an agent queries fresh code without a human re-running the indexer.
- WHERE a technology choice is open, dex prefers boring, local, well-understood
  tech (SQLite, a single binary, the filesystem) over distributed or hosted
  infrastructure.
- WHILE dex is under active development, work on it is coordinated through moongit
  issues — claim before coding, report at checkpoints — and dex is dogfooded by
  the same agents that build it.

## Non-goals

- **A cloud or multi-tenant service.** dex is single-user, single-box; it is not a
  hosted product, has no accounts, and serves no tenants but the local developer.
- **A general code host.** dex indexes and searches code; it is not a git host,
  issue tracker, or collaboration platform (moongit is that, and dex is hosted on
  it — they are separate tools).
- **Restating subsystem behavior.** The mechanics of indexing, storage, each query
  surface, embedding, MCP/HTTP serving, watching, and ignore rules live in their
  own specs; this spec governs the invariants, not the implementations.

## Checklist

- [x] Local-first: runs on the developer's box, no cloud dependency
- [x] Single Go binary CLI
- [x] MCP-primary interface for Claude Code
- [x] CGO + `sqlite_fts5` build requirement is load-bearing
- [x] Opt-in indexing — never embed an un-opted-in tree
- [x] Embedding-backend-unreachable degrades to grep-fallback, not breakage
- [x] Hot index kept current by the watch daemon
- [x] Boring/local tech preferred
- [x] Coordinated via moongit issues (dogfooding)
