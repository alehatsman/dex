# dex

Local semantic code intel for AI agents. Indexes a repo over MCP: tree-sitter
chunks → self-hosted embeddings → SQLite (vectors + BM25 FTS) with hybrid RRF
retrieval, optional cross-encoder rerank, and a call graph (type-resolved for Go;
AST-based for TypeScript, JavaScript, Python, Java, Rust).
Source never leaves your machine.

```console
$ dex search semantic ./ "where is filesystem event debouncing handled"
─── #1 markDirty  internal/watch/watch.go:60-71  (method_declaration)
```

## Quick start

```bash
git clone https://github.com/alehatsman/dex.git && cd dex
mooncake task install   # → ~/bin; safe to re-run while dex is live

dex index ./            # build the index (chunks + Go graph)
dex ask ./ "where is filesystem event debouncing handled?"
```

Requires CGO (tree-sitter + sqlite-vec) and `-tags sqlite_fts5` (BM25). Use
`mooncake task install` or `tasks.yml` — they pass both. Direct `go
build`/`go install`? Add `-tags sqlite_fts5`, `CGO_ENABLED=1`, and a C
toolchain on `PATH`.

## The headline tool: `ask`

Call `ask` before grep. One free-text question returns `semantic_hits`,
`symbols`, `suggested_reads`, a `next_action` directive, and an `avoid` line —
collapsing the grep → Read → "find references" loop into one round-trip.

Intent is inferred from the question shape (`behavior_search` / `symbol_lookup`
/ `callers` / `callees` / `architecture` / `package_topology` /
`editing_context`); pass `intent` to override.

Drop [`docs/claude-md-snippet.md`](docs/claude-md-snippet.md) into `CLAUDE.md`
to route the agent here before its grep/Read reflex. The other MCP tools are
the legs `ask` composes — call them directly only when you know which leg you
want.

## CLI reference

```bash
# query
dex ask <path> "..."                       # primary entry point (use BEFORE grep)
dex search semantic <path> "..."           # hybrid top-k chunks
dex search symbol   <path> <name>          # exact identifier lookup
dex graph neighbors <path> <file> <line>   # vector neighbours of a chunk
dex graph deps      <path> [--file|--package]
dex graph callers   <path> <name>          # incoming calls
dex graph callees   <path> <name>          # outgoing calls
dex graph links     <path> <doc>
dex graph backlinks <path> <doc>
dex graph tags      <path> --tag=<t>|--doc=<d>
dex graph export    <path>

# build / maintenance
dex index <path>           # build or refresh (--graph=off|only, --dry-run, --no-git)
dex index summarize <path> # drain pending_summaries queue
dex watch <path>           # fsnotify auto-reindex
dex reindex <path>         # drop and re-embed from scratch
dex nuke <path>            # delete the on-disk index
dex clone <src> <dst>      # seed a worktree index from a sibling
dex compact <path>         # cat all indexable files to stdout (--out, --max-bytes, --strip)

# config / setup
dex mcp                    # MCP server over stdio
dex serve [--addr] [--project]  # HTTP-MCP daemon for remote clients
dex setup                  # first-run wizard
dex config init            # scaffold .dex/config.yml
dex doctor                 # check endpoints, index, MCP wiring
dex env [--all] [--doc]    # print effective DEX_* config
dex version

# Claude Code hooks (JSON on stdin)
dex hook inject            # UserPromptSubmit → prepend ask context
dex hook rewrite           # PreToolUse(Bash) → rewrite rg/grep to dex
dex hook redirect          # PreToolUse(Read) → signatures view for big files
dex hook observe           # PostToolUse/Stop → append to hooks.jsonl
```

## Configuration

Pin config in `.dex/config.yml` (precedence: env var > file > default):

```yaml
endpoints:
  embed: http://localhost:11434   # DEX_EMBED_URL
  chat:  http://localhost:11434   # DEX_CHAT_URL
models:
  embed: mxbai-embed-large        # DEX_EMBED_MODEL
  chat:  qwen2.5-coder:14b        # DEX_CHAT_MODEL
tools:
  tier: power                     # DEX_TOOLS: ask | standard | power
index:
  include: ["cmd/", "internal/", "*.md"]  # gitignore grammar; required — no include = empty index
  ignore:  ["testdata/"]
env:                              # any DEX_* knob verbatim
  DEX_EMBED_CONCURRENCY: 8
```

Key env vars:

| Variable            | Default                          | Meaning                                    |
| ------------------- | -------------------------------- | ------------------------------------------ |
| `DEX_EMBED_URL`     | `auto`                           | OpenAI-shape `/v1/embeddings` base URL; probes ollama at localhost:11434, falls back to `http://127.0.0.1:8082` |
| `DEX_EMBED_MODEL`   | `auto`                           | Embedding model; auto-detects from ollama, falls back to `Qwen/Qwen3-Embedding-4B` |
| `DEX_INDEX_DIR`     | `~/.cache/dex`                   | Per-project index files                    |
| `DEX_CHAT_URL`      | `auto`                           | Chat completions endpoint; probes ollama, falls back to `http://127.0.0.1:8081` |
| `DEX_CHAT_MODEL`    | `auto`                           | Chat model; auto-detects from ollama, falls back to `Qwen/Qwen2.5-Coder-7B-Instruct` |
| `DEX_PROFILE`       | *(unset)*                        | Context profile: `claude`, `explore`, `bugfix`, `ci` |
| `DEX_TOOLS`         | `standard`                       | MCP surface: `ask`, `standard`, `power`    |
| `DEX_SERVE_TOKEN`   | *(unset)*                        | Bearer token for `dex serve` (env only)    |

Run `dex env --all --doc` for the full list of tuning knobs.

## MCP tool tiers

`DEX_TOOLS=ask|standard|power` (default `standard`):

- **ask** — `ask` only
- **standard** — `ask`, `ctx_*`, `search_context`, `search_workspace`, `search_grep`, `file_tree`, `file_view`
- **power** — adds `search_semantic`, `search_symbol`, `search_similar`, `graph_*`, `compress_output`, `status`, `spec_check`

`DEX_PROFILE=claude` is the recommended default for Claude Code — selects
`cl100k_base` tokenizer so token-budget reports are accurate.

## Claude Code hooks

Four hooks wire dex into Claude Code's hook events without the agent remembering
to call a tool. All fail open (3 s timeout, errors pass through untouched):

| Hook             | Event           | Effect                                                      |
| ---------------- | --------------- | ----------------------------------------------------------- |
| `inject`         | UserPromptSubmit| Prepend `ask` context to every prompt (skips < 4 words)     |
| `rewrite`        | PreToolUse/Bash | Rewrite `rg`/`grep -r` to `dex search semantic`            |
| `redirect`       | PreToolUse/Read | Redirect reads of indexed files > 400 lines to signatures view |
| `observe`        | PostToolUse/Stop| Append `{ts, tool_name, tokens}` to hooks.jsonl            |

Wiring lives in `.claude/settings.json` (committed).

## Remote access

```bash
# host
dex serve --addr :8080 --project /path/to/repo

# HTTP-MCP (preferred)
# .mcp.json: { "dex": { "type": "http", "url": "http://host:8080/v1/projects/<sha>/mcp",
#               "headers": { "Authorization": "Bearer <DEX_SERVE_TOKEN>" } } }

# stdio shim
DEX_SERVE_TOKEN=… dex mcp --remote http://host:8080 --project-id <sha>
```

## Docker

```bash
docker build -t dex .
docker run --rm -v "$PWD":/work:ro -v dex-cache:/cache \
    -e DEX_EMBED_URL=http://host.docker.internal:8082 \
    dex index /work
```

Static binary on `distroless/static` (~36 MB). Add `--user "$(id -u):$(id -g)"` for host-bound cache.

## Docs

- [`docs/vision.md`](docs/vision.md) — the capability-ladder vision (Claude / local-agent / no-GPU rungs)
- [`docs/model-selection.md`](docs/model-selection.md) — model recommendations (MTEB, VRAM, quant)
- [`docs/claude-md-snippet.md`](docs/claude-md-snippet.md) — drop-in CLAUDE.md routing block
- [`docs/observability.md`](docs/observability.md) — log field conventions
- [`docs/how-dex-guide-works.md`](docs/how-dex-guide-works.md) — how `dex guide` works
- [`specs/`](specs/) — living specs for indexing, search, graph, MCP, storage

## License

MIT — see [LICENSE](./LICENSE).
