# dex

Local semantic code search for [Claude Code](https://docs.claude.com/en/docs/claude-code)
and the terminal. `dex` indexes a repo (chunks + embeddings + a call/import
graph) and serves `ask` / `find` / `trace` and graph navigation as MCP tools,
so an agent reaches for one dex tool instead of grepping blind.

## Install

```sh
git clone https://github.com/alehatsman/dex.git && cd dex
mooncake task install        # builds with -tags sqlite_fts5 (CGO), installs ~/.local/bin/dex
```

Needs Go and a C toolchain (the build links SQLite FTS5 + tree-sitter). No
mooncake? `CGO_ENABLED=1 go install -tags sqlite_fts5 ./cmd/dex`.

## Backends

- **Embeddings** (semantic search): an OpenAI-compatible or ollama server —
  defaults to `http://localhost:11434`. Override with `DEX_EMBED_URL` /
  `DEX_EMBED_MODEL`.
- **Chat** (optional): powers `ask` answer synthesis and `read --mode=summary`.
  Set `DEX_CHAT_URL` / `DEX_CHAT_MODEL`; without it those degrade gracefully.
- **No GPU?** `DEX_EMBED_ENGINE=none` runs the *lean profile*: BM25 + exact
  symbol + call-graph lanes, zero inference required.

## Use it with Claude Code

```sh
cd your-project
dex setup                              # guided: checks backends, indexes, wires up MCP
```

Or do it by hand:

```sh
dex config init                        # scaffold .dex/config.yml (indexes the whole tree by default)
dex index .                            # build the index (chunks + graph)
claude mcp add --scope user dex -- dex mcp
```

`dex doctor` verifies the whole setup end-to-end. The agent then calls dex
tools (`ask`, `find`, `read`, …) automatically.

## Verbs

For the everyday set the CLI verbs and the MCP tool names are identical. CLI
form is `dex <verb> [path] <args…>`; `path` defaults to the current directory.
The graph/analysis tools are flat MCP tools but live under `dex graph <sub>` on
the CLI (`deps callers callees path diff clusters cycles smells routes export`,
each annotated `(MCP: <name>)` in `dex graph --help`).

| verb     | what it does                                                        |
|----------|---------------------------------------------------------------------|
| `ask`    | one-shot router: picks intent, fuses lanes, returns suggested reads + a cited answer |
| `find`   | hybrid semantic top-k search — fuses exact symbol-name hits via RRF |
| `map`    | deterministic repo orientation map                                  |
| `trace`  | call graph — `--dir callers\|callees\|path\|impact` (impact = transitive caller blast-radius) |
| `locate` | one-call orientation around a `ref` (`path:line`) / `symbol` / `frame`: callers, tests, nearest doc, last commit, notes |
| `review` | per-hunk PR intelligence for a `ref` / `branch` / `pr`: touched symbols, callers + risk, tests, churn, author history, notes |
| `refactor` | plan a type-precise rename → byte-exact edit triples you apply (never writes); Go-only v1 |
| `read`   | read a file — `--mode full` (raw, default), `signatures`, `skeleton`, `map`, `summary` (LLM), … |
| `grep`   | exact regex match                                                   |
| `ls`     | file-tree listing                                                   |
| `shell`  | run a command with compressed output                                |

```sh
dex ask "where is filesystem event debouncing handled?"
dex find . "retry logic"
dex trace . Run --dir callers
```

Start with `ask` — it routes the query and tells you what to read next. Every
verb works on the CLI. As MCP tools the everyday set is `ask find map trace
locate review refactor read grep ls shell notes`; the rest (`deps diff
clusters routes smells cohort status budget session checkpoint`) is behind
`DEX_EXPERT=1` to keep the agent's tool list small.
Call-graph walks fold into `trace --dir callers|callees|path|impact` — there are
no standalone `callers`/`callees`/`path`/`impact` MCP tools (callers/callees/path
remain `dex graph` subs; impact rides `trace`).

## Config

Per-project settings live in `.dex/config.yml`; any `DEX_*` env var overrides
them. Run `dex env` to print the effective configuration, `dex help` for the
full command reference.

## Docs

- [architecture.md](docs/architecture.md) — how dex indexes and retrieves
- [tools.md](docs/tools.md) — the tool/verb surface and response contract
- [deployment.md](docs/deployment.md) — backends, profiles, model selection
- [config.md](docs/config.md) — `.dex/config.yml` and `DEX_*` reference
