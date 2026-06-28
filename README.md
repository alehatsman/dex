# dex

Local semantic code search for [Claude Code](https://docs.claude.com/en/docs/claude-code)
and the terminal. `dex` indexes a repo (chunks + embeddings + a call/import
graph) and serves tools over MCP. An agent calls `brief(task)` before any
coding task to get a curated context pack (ranked files, local rules, sibling
tests), then navigates with `ask` / `search` / `trace` instead of grepping blind.

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

CLI form is `dex <verb> [path] <args…>`; `path` defaults to the current
directory. MCP tool names differ from CLI verbs for several commands (see
below); the agent sees the MCP name.

| CLI verb   | MCP tool name   | what it does |
|------------|-----------------|--------------|
| `brief`    | `brief`         | **start here** — task-specific context pack: ranked files, local rules, sibling tests, suggested reads |
| `ask`      | `ask`           | one-shot router: picks intent, fuses lanes, returns suggested reads + a cited answer |
| `find`     | `search`        | hybrid semantic top-k search — fuses exact symbol-name hits via RRF |
| `map`      | `repo_map`      | deterministic repo orientation map |
| `trace`    | `trace`         | call graph — `--dir callers\|callees\|path\|impact` |
| `locate`   | `locate`        | one-call orientation around a `ref` / `symbol` / `frame`: callers, tests, nearest doc, last commit, notes |
| `review`   | `review_diff`   | per-hunk PR intelligence: touched symbols, callers + risk, tests, churn, author history, notes |
| `verify`   | `verify_change` | run the tests a change implicates → pass/fail; Go-only v1 |
| `read`     | `read`          | read a file — `--mode full` (raw, default), `signatures`, `skeleton`, `map`, `summary` (LLM), … |
| `grep`     | `grep`          | exact regex match |
| `shell`    | `shell`         | run a command with compressed output |
| `notes`    | `notes`         | persistent project memory: `add`/`list`/`delete`/`gc` facts |

```sh
dex brief . "add OAuth support"       # start here — curated context pack
dex ask "where is rate limiting?"     # open-ended question
dex find . "retry logic"
dex trace . Run --dir callers
```

Call `brief(task)` at the start of every coding task — it returns ranked files
to read, local rules, and sibling tests so you don't fan out blindly. Use `ask`
for open-ended questions mid-task.

Every verb works on the CLI. The default MCP surface is `brief ask search
repo_map trace locate review_diff verify_change read grep shell notes`. The
power lane (`plan_rename rehearse_patch check deps diff clusters routes smells
cohort status budget session checkpoint`) is behind `DEX_EXPERT=1`.
The graph/analysis tools live under `dex graph <sub>` on the CLI
(`deps callers callees path diff clusters cycles smells routes export`,
each annotated `(MCP: <name>)` in `dex graph --help`).

## Config

Per-project settings live in `.dex/config.yml`; any `DEX_*` env var overrides
them. Run `dex env` to print the effective configuration, `dex help` for the
full command reference.

## Docs

- [architecture.md](docs/architecture.md) — how dex indexes and retrieves
- [tools.md](docs/tools.md) — the tool/verb surface and response contract
- [deployment.md](docs/deployment.md) — backends, profiles, model selection
- [config.md](docs/config.md) — `.dex/config.yml` and `DEX_*` reference
