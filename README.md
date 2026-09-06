# dex

Local semantic code search for [Claude Code](https://docs.claude.com/en/docs/claude-code) and the terminal.

Indexes your repo — chunks, symbols, embeddings, call graph — and exposes a small MCP surface so Claude finds the right code before editing.

Claude arrives at a repo blind. `dex` gives it a fast local map: ranked files, callers, tests, and conventions for the task at hand.

```text
query(task) → edit → query("review my changes")
```

## Install

Requires Go and a C toolchain (SQLite FTS5, tree-sitter).

```sh
git clone https://github.com/alehatsman/dex.git && cd dex
mooncake task install   # builds with -tags sqlite_fts5, installs to ~/.local/bin/dex
```

Without mooncake:

```sh
CGO_ENABLED=1 go install -tags sqlite_fts5 ./cmd/dex
```

## Setup

```sh
cd your-project
dex setup
```

`dex setup` creates `.dex/config.yml`, indexes the repo, and registers the MCP server with Claude Code.

Manual:

```sh
dex config init
dex index .
claude mcp add --scope user dex -- dex mcp
dex doctor
```

## MCP tools

The agent-facing surface is **one verb**, constant across every deployment
profile (full / bm25-only / lean):

| Verb | What it does |
|---|---|
| `query` | The one read verb. Input **shape** picks the lane and the answer's precision tracks it: a path → its compressed signatures, a `path:line` or range → that slice, a `/regex/` → grep, a bare symbol (`NewServer`, `(*Server).Run`) → just its call graph, a prose question → a ranked semantic evidence pack with a `next_action`. Force the lane with `kind=…`, the facet with `want=…` (`want=assemble` for a task-start working set, `kind=review` to review the working tree). |

`query` is advisory intelligence, not a file server — a bare path returns
compressed signatures; for raw bytes use the native Read tool. Running commands
(builds / tests / git) is the host agent's job, not dex's. dex is retrieval over
the codebase, not agent memory: durable findings live in the harness's own
file-based memory, not in dex (the `record` verb was removed in #205).

Everything else — the raw `search`, `trace`, `locate`, `grep`, `read` primitives
plus the `clusters` / `smells` / `routes` graph-wide reports — is a power-lane
overlay behind `DEX_EXPERT=1`. `query` covers everyday work; the overlay
never changes its shape. (`deps`/`cohort`/`refs`/`clones`/`similar`/`check`
are reachable only via `query(kind=...)` — no standalone tool of their own,
#852.)

## CLI

```sh
dex query [--kind=K] [--want=W] [path] <input...>
```

`path` defaults to `.`. The CLI collapsed onto `query` too (#849) — there is no
`dex ask`/`search`/`read`/`locate`/`trace`/`review_diff`/`check`/`grep` verb
anymore; each is a `--kind=` value on the one read verb, same as MCP's `query`
tool. `--kind` is optional for the shape-detected lanes (a path, `path:line`,
`/regex/`, a bare symbol, or prose all route themselves); force it for the
lanes with no natural shape (`check`, `refs`, `cohort`, `deps`, `status`).

| Command | Purpose |
|---|---|
| `dex setup` | Config, index, MCP wiring |
| `dex doctor` | Verify install/backend/index/MCP |
| `dex env` | Print effective config |
| `dex config init` | Create `.dex/config.yml` |
| `dex index [path]` | Build/update index |
| `dex index status [path]` | Show index freshness |
| `dex watch [path]` | Keep index current |
| `dex query [path] <question>` | Task pack / open-ended repo question |
| `dex query --kind=search [path] <query>` | Hybrid semantic + BM25 + symbol + graph search |
| `dex query <file\|symbol>` | Read exact context |
| `dex query --kind=locate [path] <ref>` | Orient around one object |
| `dex query --kind=callers\|callees\|impact\|path [path] <symbol>` | Call-graph traversal |
| `dex query --kind=review [path]` | Per-hunk change analysis (working tree) |
| `dex query --kind=check [path] <ref...>` | Verify file:line[:symbol] references |
| `dex query --kind=grep [path] <regex>` | Regex search |
| `dex mcp` | Serve MCP |

```sh
dex query . "add OAuth support"
dex query --kind=search . "retry logic"
dex query --kind=locate . AuthMiddleware
dex query --kind=callers . Run
dex query --kind=review .
```

Run `dex help all` for the full lane reference (`refs`/`cohort`/`deps`,
`plan_rename`/`rehearse_patch`, `dex graph <sub>`) and the capabilities the
collapse deliberately dropped from the CLI (still reachable as MCP tools under
`DEX_EXPERT=1`).

## Backends

### Embeddings

Semantic search requires an OpenAI-compatible or Ollama embedding server. Default: `http://localhost:11434`.

```sh
DEX_EMBED_URL=...
DEX_EMBED_MODEL=...
```

### Chat

Optional. Used by `dex query`'s prose lane (synthesized answers) and the MCP `read` tool's `summary` mode. Without it those degrade to lexical/structural results instead of failing.

```sh
DEX_CHAT_URL=...
DEX_CHAT_MODEL=...
```

### No embeddings

BM25 + exact symbol + call graph only, no inference required:

```sh
DEX_EMBED_ENGINE=none
```

## Expert tools

```sh
DEX_EXPERT=1
```

Overlays the power lanes onto `query`: the raw primitives `search`,
`trace`, `locate`, `grep`, `read` plus the graph/quality reports `clusters`,
`routes`, `smells`, `status`, `repo_map`, `review_diff`, `plan_rename`, and
`rehearse_patch`. `deps`, `cohort`, `refs`, `clones`, `similar`, and `check`
are not separate tools — each is a `query(kind=...)` value (#852), covered
with full fidelity by the everyday surface.

`dex graph <sub>` subcommands work without the flag on the CLI regardless —
some (`neighbors`, `packages`, `links`, `backlinks`, `tags`, `cycles`, `diff`,
`export`) have no MCP tool at all; others (`similar`, `clones`, `routes`,
`smells`, `clusters`) are the CLI door onto the same-named DEX_EXPERT MCP
tool. `dex graph deps` is the one that's gone — that's `dex query --kind=deps`
now (#849).

```sh
dex graph cycles
dex graph export
```

## Docs

- [architecture.md](docs/architecture.md) — indexing and retrieval internals
- [tools.md](docs/tools.md) — full tool surface and response contract
- [deployment.md](docs/deployment.md) — backends, profiles, model selection
- [config.md](docs/config.md) — `.dex/config.yml` and `DEX_*` env reference
