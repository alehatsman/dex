# dex

Local semantic code search for [Claude Code](https://docs.claude.com/en/docs/claude-code)
and the terminal.

`dex` indexes your repo — chunks, symbols, embeddings, and a call/import graph —
then exposes a small MCP surface that helps Claude find the right code before it
starts editing.

The core idea:

```text
brief(task) -> read exact context -> edit -> review_diff -> verify_change
```

Dex is not another coding agent. It is local repo intelligence for the agent you
already use.

## Why

Claude is strong at editing, but weak at knowing your repo before it has read
enough files. Dex gives it a fast local map:

- ranked files and symbols for a task
- local conventions and nearby examples
- sibling tests and likely checks
- callers/callees and blast-radius hints
- cited repo facts with index freshness
- compressed command output when needed

That means less blind grep, less prompt waste, and fewer edits made from guessed
context.

## Quick start

```sh
git clone https://github.com/alehatsman/dex.git && cd dex
mooncake task install
```

This builds with `-tags sqlite_fts5` and installs `~/.local/bin/dex`.

Needs Go and a C toolchain. The build links SQLite FTS5 and tree-sitter.

No mooncake:

```sh
CGO_ENABLED=1 go install -tags sqlite_fts5 ./cmd/dex
```

Set up a project:

```sh
cd your-project
dex setup
```

`dex setup` checks backends, indexes the repo, and wires up MCP for Claude Code.

Manual setup:

```sh
dex config init
dex index .
claude mcp add --scope user dex -- dex mcp
```

Verify everything:

```sh
dex doctor
```

## Agent workflow

For coding tasks, the agent should use Dex like this:

```text
1. brief(task)
2. read suggested files
3. locate key symbols
4. search only if context is missing
5. trace if impact/path is unclear
6. edit
7. review_diff(staged)
8. verify_change(staged)
```

`brief(task)` is the normal starting point. It returns the smallest useful
context pack for the task: ranked files, symbols, local rules, sibling tests,
risks, and suggested reads.

## Default MCP tools

The default MCP surface is intentionally small:

| Tool | Purpose |
|---|---|
| `brief` | task-specific context pack; start here |
| `search` | hybrid semantic + exact + symbol + graph search |
| `read` | fetch exact file/symbol context |
| `locate` | definition, callers, tests, docs, notes, ownership around one ref |
| `trace` | callers/callees/path/impact graph traversal |
| `review_diff` | per-hunk change intelligence for staged/ref/branch/diff |
| `verify_change` | select or run implicated tests/checks |
| `notes` | scoped, cited repo-local memory |
| `shell` | run commands with compressed output |

`shell` is a utility tool, not a code intelligence primitive.

`ask`, `repo_map`, and `grep` are not in the default MCP surface. They are useful
escape hatches, but the agent should prefer deterministic navigation through
`brief`, `search`, `read`, `locate`, and `trace`.

## CLI

CLI form:

```sh
dex <verb> [path] <args...>
```

`path` defaults to the current directory.

| Command | Purpose |
|---|---|
| `dex setup` | guided setup: config, index, MCP wiring |
| `dex doctor` | verify install/backend/index/MCP health |
| `dex env` | print effective config |
| `dex config init` | create `.dex/config.yml` |
| `dex index [path]` | build/update index |
| `dex index status [path]` | show index freshness |
| `dex index watch [path]` | keep index fresh |
| `dex brief [path] <task...>` | task-specific context pack |
| `dex ask [path] <question...>` | open-ended repo question; human convenience |
| `dex find [path] <query...>` | hybrid semantic/symbol/text search |
| `dex read <file\|symbol>` | read exact context |
| `dex locate [path] <ref\|symbol\|path:line>` | orient around one object |
| `dex trace [path] <symbol>` | callers/callees/path/impact graph |
| `dex review [path]` | per-hunk change intelligence |
| `dex verify [path]` | select/run implicated checks |
| `dex notes add\|list\|delete\|gc` | cited repo-local memory |
| `dex grep [path] <regex>` | CLI-only regex escape hatch |
| `dex shell -- <cmd...>` | run command with compressed output |
| `dex mcp` | serve MCP tools |

Examples:

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

Semantic search uses an OpenAI-compatible or Ollama embedding server.

Default:

```text
http://localhost:11434
```

Override with:

```sh
DEX_EMBED_URL=...
DEX_EMBED_MODEL=...
```

### Chat

Chat is optional. It powers answer synthesis and summaries:

- `dex ask`
- `dex read --mode summary`

Configure with:

```sh
DEX_CHAT_URL=...
DEX_CHAT_MODEL=...
```

Without chat configured, those features degrade gracefully.

### Lean profile

No GPU or embedding backend:

```sh
DEX_EMBED_ENGINE=none
```

This runs BM25 + exact symbol + call-graph lanes only. Zero inference required.

## Expert tools

Advanced MCP tools are hidden behind:

```sh
DEX_EXPERT=1
```

Expert mode is for lower-level graph and planning tools such as `repo_map`,
`grep`, `plan_rename`, `rehearse_patch`, `graph_*`, `risk_scan`,
`similar_changes`, and `context_budget`.

Most users and agents should not need them.

Low-level graph analysis is also available from the CLI:

```sh
dex graph <subcommand>
```

Examples:

```sh
dex graph deps
dex graph cycles
dex graph routes
dex graph export
```

## Output contract

MCP responses are structured and cited. Repo claims include `path:line` evidence,
confidence, index freshness, token estimate, and suggested next calls.

See [`docs/tools.md`](docs/tools.md) for the full response contract.

## Notes

`notes` stores repo-local memory. Notes should be scoped, cited, and
confidence-tagged; they should not become free-form magical memory.

See [`docs/tools.md`](docs/tools.md) for the note schema.

## Config

Per-project config lives in `.dex/config.yml`. Environment variables override
config.

Useful commands:

```sh
dex env
dex help
```

## Docs

- [architecture.md](docs/architecture.md) — how dex indexes and retrieves
- [tools.md](docs/tools.md) — CLI/MCP tool surface and response contract
- [deployment.md](docs/deployment.md) — backends, profiles, model selection
- [config.md](docs/config.md) — `.dex/config.yml` and `DEX_*` reference
