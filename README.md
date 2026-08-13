# dex

Local semantic code search for [Claude Code](https://docs.claude.com/en/docs/claude-code) and the terminal.

Indexes your repo — chunks, symbols, embeddings, call graph — and exposes a small MCP surface so Claude finds the right code before editing.

Claude arrives at a repo blind. `dex` gives it a fast local map: ranked files, callers, tests, and conventions for the task at hand.

```text
ask(task) → look(target) → edit → ask("review my changes")
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

The agent-facing surface is **four verbs**, constant across every deployment
profile (full / bm25-only / lean):

| Verb | What it does |
|---|---|
| `ask` | Front door for any task. Routes intent, fuses semantic + symbol + graph lanes, returns a ranked evidence pack and a `next_action`. `ask("review my changes")` reviews your working tree; `intent=assemble` returns a budget-bounded working set. |
| `look` | Exact fetch once you can name the target: a path → read, a `/regex/` → grep, a `path:line` → locate, a symbol → its call graph. |
| `act` | Run builds / tests / git; compressed output inside the universal `{result, trust, cost, next}` envelope. |
| `remember` | Durable project memory: write a fact, recall by query, or correct a stale one with `supersedes`. |

Everything else — the raw `search`, `trace`, `locate`, `grep`, `read`, `shell`
primitives plus the `deps` / `clusters` / `smells` / `clones` graph lanes — is a
power-lane overlay behind `DEX_EXPERT=1`. The four verbs cover everyday work; the
overlay never changes their shape.

## CLI

```sh
dex <verb> [path] <args>
```

`path` defaults to `.`. The CLI keeps the granular verbs (they map onto the four
MCP verbs' lanes — `dex search`/`read`/`locate`/`trace` are what `ask` and `look`
route to internally).

| Command | Purpose |
|---|---|
| `dex setup` | Config, index, MCP wiring |
| `dex doctor` | Verify install/backend/index/MCP |
| `dex env` | Print effective config |
| `dex config init` | Create `.dex/config.yml` |
| `dex index [path]` | Build/update index |
| `dex status [path]` | Show index freshness |
| `dex watch [path]` | Keep index current |
| `dex ask [path] <question>` | Task pack / open-ended repo question |
| `dex search [path] <query>` | Hybrid semantic + BM25 + symbol + graph search |
| `dex read <file\|symbol>` | Read exact context |
| `dex locate [path] <ref>` | Orient around one object |
| `dex trace [path] <symbol>` | Callers/callees/impact |
| `dex review [path]` | Per-hunk change analysis |
| `dex verify [path]` | Select/run implicated checks |
| `dex notes add\|list\|delete\|gc` | Repo-local memory |
| `dex grep [path] <regex>` | Regex search |
| `dex mcp` | Serve MCP |

```sh
dex ask . "add OAuth support"
dex search . "retry logic"
dex locate . AuthMiddleware
dex trace . Run --dir callers
dex review . --staged
dex verify . --staged
```

## Backends

### Embeddings

Semantic search requires an OpenAI-compatible or Ollama embedding server. Default: `http://localhost:11434`.

```sh
DEX_EMBED_URL=...
DEX_EMBED_MODEL=...
```

### Chat

Optional. Used by `dex ask` and `dex read --mode summary`. Without it those commands are unavailable.

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

Overlays the power lanes onto the four verbs: the raw primitives `search`,
`trace`, `locate`, `grep`, `read`, `shell` plus the graph/quality lanes `deps`,
`clusters`, `routes`, `smells`, `clones`, `similar`, `cohort`, `session`, and the
full `notes` knowledge surface.

Graph tools are also available from the CLI without the flag:

```sh
dex graph deps
dex graph cycles
dex graph routes
dex graph export
```

## Docs

- [architecture.md](docs/architecture.md) — indexing and retrieval internals
- [tools.md](docs/tools.md) — full tool surface and response contract
- [deployment.md](docs/deployment.md) — backends, profiles, model selection
- [config.md](docs/config.md) — `.dex/config.yml` and `DEX_*` env reference
