# dex

Local semantic code search for [Claude Code](https://docs.claude.com/en/docs/claude-code)
and the terminal.

`dex` indexes a repo — chunks, symbols, embeddings, and a call/import graph —
then serves a small MCP tool surface for coding agents.

Normal agent flow:

1. call `brief(task)` before coding
2. read the ranked files/symbols it recommends
3. use `search`, `locate`, and `trace` only when more context is needed
4. use `review_diff` and `verify_change` before finalizing changes

Dex is not another agent. Dex gives Claude precise local context so Claude does
not grep blindly or waste tokens guessing.

## Install

```sh
git clone https://github.com/alehatsman/dex.git && cd dex
mooncake task install
```

This builds with `-tags sqlite_fts5` and installs `~/.local/bin/dex`.

Needs Go and a C toolchain. The build links SQLite FTS5 and tree-sitter.

Without mooncake:

```sh
CGO_ENABLED=1 go install -tags sqlite_fts5 ./cmd/dex
```

## Backends

### Embeddings

Semantic search uses an OpenAI-compatible or Ollama server.

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

Chat is optional. It powers:

- `ask`
- `read --mode summary`

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

## Use with Claude Code

Recommended:

```sh
cd your-project
dex setup
```

`dex setup` checks backends, indexes the repo, and wires up MCP.

Manual setup:

```sh
dex config init
dex index .
claude mcp add --scope user dex -- dex mcp
```

Verify:

```sh
dex doctor
```

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
| `dex brief [path] <task...>` | task-specific context pack; start here |
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

## Default MCP surface

The default MCP surface is intentionally small:

```text
brief
search
read
locate
trace
review_diff
verify_change
notes
shell
```

| MCP tool | Purpose |
|---|---|
| `brief` | start every coding task; returns ranked files, local rules, sibling tests, risks, suggested reads |
| `search` | hybrid semantic + exact + symbol + graph search |
| `read` | fetch exact file/symbol context |
| `locate` | definition, callers, callees, tests, docs, notes, ownership around one ref |
| `trace` | graph traversal: callers, callees, path, impact |
| `review_diff` | review staged/ref/branch/diff with per-hunk risk |
| `verify_change` | select or run implicated tests/checks |
| `notes` | scoped, cited repo-local memory |
| `shell` | run commands with compressed output |

`shell` is a utility tool, not a code intelligence primitive.

`ask`, `repo_map`, and `grep` are not part of the default MCP surface. They are
useful escape hatches, but the agent should prefer deterministic navigation
through `brief`, `search`, `read`, `locate`, and `trace`.

## Agent workflow

For coding tasks, agents should use this sequence:

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

## Expert tools

Advanced MCP tools are hidden behind:

```sh
DEX_EXPERT=1
```

Expert tools may include:

```text
ask
repo_map
grep
plan_rename
rehearse_patch
graph_deps
graph_diff
graph_cycles
graph_routes
graph_clusters
risk_scan
similar_changes
context_budget
```

Most users and agents should not need them.

Low-level graph analysis is available from the CLI under:

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

## Response contract

All MCP tools return cited, structured output.

Every repo claim must have evidence:

```json
{
  "summary": "...",
  "evidence": [
    {
      "path": "internal/foo.go",
      "lines": [12, 40],
      "kind": "definition"
    }
  ],
  "confidence": 0.86,
  "index_status": {
    "fresh": true,
    "dirty_files": [],
    "warnings": []
  },
  "token_estimate": 1200,
  "next_calls": [
    {
      "tool": "read",
      "args": {
        "target": "internal/foo.go",
        "mode": "skeleton"
      },
      "reason": "Need exact implementation before editing"
    }
  ]
}
```

## Notes

Project notes are structured memory, not free-form magic.

A note should include:

```json
{
  "fact": "Indexer refresh is triggered through Claude Code hooks.",
  "scope": "repo|path|symbol|task",
  "source": "user|review|test|commit|manual",
  "evidence": [
    {
      "path": "internal/hooks/refresh.go",
      "lines": [10, 42]
    }
  ],
  "confidence": 0.8,
  "expires": null
}
```

## Config

Per-project config lives in:

```text
.dex/config.yml
```

Environment variables override config.

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
