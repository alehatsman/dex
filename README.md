# dex

Local semantic code search for [Claude Code](https://docs.claude.com/en/docs/claude-code) and the terminal.

Indexes your repo — chunks, symbols, embeddings, call graph — and exposes a small MCP surface so Claude finds the right code before editing.

Claude arrives at a repo blind. `dex` gives it a fast local map: ranked files, callers, tests, and conventions for the task at hand.

```text
brief(task) → read → edit → review_diff → verify_change
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

| Tool | What it does |
|---|---|
| `brief` | Ranked files, symbols, rules, tests, and risks for a task. Start here. |
| `search` | Hybrid: semantic + BM25 + symbol + graph |
| `read` | Exact file or symbol context |
| `locate` | Definition, callers, tests, notes around one ref |
| `trace` | Callers/callees/path/impact graph |
| `review_diff` | Per-hunk analysis of staged/ref/branch diff |
| `verify_change` | Select or run tests implicated by staged changes |
| `notes` | Scoped, cited repo-local memory |
| `shell` | Run commands with compressed output |

`ask`, `repo_map`, and `grep` are hidden by default. Enable with `DEX_EXPERT=1` when you need raw graph or planning tools.

## CLI

```sh
dex <verb> [path] <args>
```

`path` defaults to `.`.

| Command | Purpose |
|---|---|
| `dex setup` | Config, index, MCP wiring |
| `dex doctor` | Verify install/backend/index/MCP |
| `dex env` | Print effective config |
| `dex config init` | Create `.dex/config.yml` |
| `dex index [path]` | Build/update index |
| `dex index status [path]` | Show index freshness |
| `dex index watch [path]` | Keep index current |
| `dex brief [path] <task>` | Task context pack |
| `dex ask [path] <question>` | Open-ended repo question |
| `dex find [path] <query>` | Hybrid search |
| `dex read <file\|symbol>` | Read exact context |
| `dex locate [path] <ref>` | Orient around one object |
| `dex trace [path] <symbol>` | Callers/callees/impact |
| `dex review [path]` | Per-hunk change analysis |
| `dex verify [path]` | Select/run implicated checks |
| `dex notes add\|list\|delete\|gc` | Repo-local memory |
| `dex grep [path] <regex>` | Regex search |
| `dex shell -- <cmd>` | Command with compressed output |
| `dex mcp` | Serve MCP |

```sh
dex brief . "add OAuth support"
dex find . "retry logic"
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

Unlocks: `repo_map`, `ask`, `grep`, `plan_rename`, `rehearse_patch`, `graph_*`, `risk_scan`, `similar_changes`, `context_budget`.

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
