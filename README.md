# dex

**Local semantic code intel for AI agents — so they stop grepping blind.**

dex is a single Go binary that indexes a repo and serves it over MCP:
tree-sitter chunks → self-hosted embeddings → SQLite (vectors + BM25
FTS) with hybrid RRF retrieval, an optional cross-encoder rerank, and a
type-resolved Go call graph. One `ask` call gives an agent the intent
routing, the top-k chunks, the symbol hits, and the call sites — each
with enough inline content that it usually doesn't need a follow-up
Read. Source never leaves your machine.

```console
$ dex search semantic ./ "where is filesystem event debouncing handled"
─── #1 markDirty  internal/watch/watch.go:60-71  (method_declaration)
// markDirty resets the debounce timer; on expiry it runs an index pass.
func (w *Watcher) markDirty() { … }
```

Ask the same thing through MCP and the agent gets the file pinned, a
file-level summary, a `next_action` directive, and an `avoid` line
telling it *not* to re-grep — collapsing the grep → Read → "find
references" loop into one round-trip.

## Who it's for

- **Agent developers** — give Claude (or any MCP client) a semantic
  retrieval tool that returns ranked, summarized, graph-aware results
  instead of a wall of grep hits. The `ask` tool routes intent and
  composes the bundle so the agent doesn't have to.
- **Solo developers on private code** — semantic search over your repo
  with embeddings you host yourself (ollama / vLLM / TEI, local or
  SSH-tunneled). No code, no chunks, no queries leave the box.
- **Anyone drowning in a large Go codebase** — type-resolved
  callers/callees, package topology, and "what calls this" without an
  IDE, served the same way to the CLI and to an agent.

## Quick start

```bash
git clone https://github.com/alehatsman/dex.git && cd dex
mooncake task install   # → ~/.local/bin, no sudo; atomic rename-swap, so
                        #   it's safe to re-run while dex mcp/watch is live

dex index ./            # build the index (chunks + Go graph)
dex ask ./ "where is filesystem event debouncing handled?"
```

Or via the [`dex` dotfiles component](https://github.com/alehatsman/dotfiles/tree/main/components/dex),
which wires the embedding endpoint, SSH tunnel, and MCP registration
for you.

dex needs CGO (tree-sitter grammars + the embedded sqlite-vec
extension) and the `sqlite_fts5` build tag (FTS5 powers the BM25 leg).
The `tasks.yml` and Dockerfile pass both — use them. Invoking `go
build` / `go install` directly? Add `-tags sqlite_fts5`, set
`CGO_ENABLED=1`, and have a C toolchain on `PATH`.

## The headline tool: `ask`

`ask` is what an agent should reach for first — whenever it would
otherwise start with a broad grep, a speculative Read, or a "find
references" fan-out. One free-text question returns one compact bundle:
`semantic_hits`, `symbols`, `suggested_reads`, plus a prose
`next_action` directive and an `avoid` line. Intent
(`behavior_search` / `symbol_lookup` / `callers` / `callees` /
`architecture` / `package_topology` / `editing_context`) is inferred
from the question shape; pass `intent` to override.

Drop [`docs/claude-md-snippet.md`](docs/claude-md-snippet.md) into your
`CLAUDE.md` to route the agent here before its grep/Read reflex kicks
in. The other MCP tools are the legs `ask` composes — call them
directly only when you already know which leg you want.

**Example 1 — free-text behaviour search.** *"where is filesystem
event debouncing handled?"* auto-routes to `behavior_search` and pins
the file from the question alone:

```jsonc
"suggested_reads": [
  { "path": "internal/watch/watch.go", "start_line": 1, "end_line": 215,
    "reason": "top semantic match",
    "content": "Implements a Watcher that re-indexes a project on
                fsnotify events using debounced timers..." }
],
"next_action": "Read internal/watch/watch.go to ground your answer.",
"avoid":       "Do not read entire files; the suggested ranges cover
                the relevant context."
```

No grep, no exploratory Read — the agent jumps straight to the named
range with a file-level summary already in hand.

**Example 2 — symbol with an explicit verb.** *"callers of
buildNextAction"* auto-routes to `callers`; the bundle ships call sites
pre-resolved by the static graph's `calls` edges (Go) or a ripgrep pass
(other languages):

```jsonc
"symbols":    [{ "path": "internal/mcp/context.go", "qualified_name": "buildNextAction",
                 "start_line": 766, "end_line": 815 }],
"references": [
  { "path": "internal/mcp/context.go",      "line": 341,
    "snippet": "out.NextAction = buildNextAction(intent, out.SuggestedReads, ...)" },
  { "path": "internal/mcp/context_test.go", "line": 597,
    "snippet": "got := buildNextAction(tc.intent, tc.reads, tc.syms, tc.topSem)" }
],
"avoid": "Do not grep for the identifier — the `references` field
          already lists usages."
```

The declaration and every usage come back in one round-trip; the
`avoid` line tells the agent not to second-guess it with grep.

## What you can do

The query-side CLI mirrors the MCP tool surface 1:1 — `dex ask` ↔
`ask`, `dex search semantic` ↔ `search_semantic`, `dex graph callers`
↔ `graph_callers` — so the CLI and an agent feel like the same tool.

```bash
# query (mirrors MCP tools)
dex ask <path> "..."                       # primary entry point (use BEFORE grep)
                                           #   --intent --k --no-inline --format=text|json
dex search semantic <path> "..."           # hybrid top-k chunks
                                           #   --k --rerank=off --explain --format=json
dex search symbol   <path> <name>          # exact identifier lookup
dex graph neighbors <path> <file> <line>   # vector neighbours of a chunk
dex graph deps      <path> [--file=<rel>|--package=<full>]
dex graph callers   <path> <name>          # incoming calls edges (Go-only)
dex graph callees   <path> <name>          # outgoing calls edges (Go-only)
dex graph links     <path> <doc>           # markdown docs this doc links to
dex graph backlinks <path> <doc>           # markdown docs that link to this doc
dex graph tags      <path> --tag=<t>|--doc=<d>  # tag→docs (clustering) or doc→tags
dex graph export    <path>                 # dump nodes.jsonl + edges.jsonl
dex view summarize  <path> <file>          # one-shot file/range gist via chat
dex index status    [<path>]               # endpoint health + indexed projects

# build / maintenance (CLI-only)
dex index <path>           # build or refresh (chunks + Go graph)
                           #   --graph=off  skip graph phase
                           #   --graph=only refresh just the graph
dex index summarize <path> # drain pending_summaries queue
dex generate <path> "..."  # RAG: top-k chunks → chat endpoint
dex watch <path>           # fsnotify-driven auto-reindex
dex clone <src> <dst>      # seed a worktree's index from a sibling
dex reindex <path>         # drop and re-embed from scratch
dex nuke <path>            # delete the on-disk index
dex mcp                    # MCP server over stdio
```

`dex env` prints effective config with sources; `dex -h` lists
everything.

When running as `dex mcp`, the server registers:

| Tool              | Always on? | What it does                                                                |
| ----------------- | ---------- | --------------------------------------------------------------------------- |
| `ask`             | yes        | **Primary entry point.** Router + composed bundle.                          |
| `search_semantic` | yes        | Hybrid (cosine + BM25 + optional rerank) top-k chunks. Supports `exclude`.  |
| `search_symbol`   | yes        | Exact identifier lookup (SQL scan, no embedding).                           |
| `graph_neighbors` | yes        | Vector neighbours of a known chunk at `path:start_line`.                    |
| `graph_deps`      | yes        | `imports` edges for a file or package. Sourced from the static graph.       |
| `graph_callers`   | yes        | Incoming `calls` edges (Go-only today).                                     |
| `graph_callees`   | yes        | Outgoing `calls` edges (Go-only today).                                     |
| `graph_links`     | yes        | Outgoing markdown `links`/`wikilinks` — docs a doc points to.               |
| `graph_backlinks` | yes        | Incoming markdown `links`/`wikilinks` — what links to a doc (Obsidian-style).|
| `graph_tags`      | yes        | Tag graph: `tag`→documents (ranked) or `doc`→tags. Tag-based clustering.     |
| `index_status`    | yes        | Endpoint health (embed / chat / rerank) + indexed projects.                 |
| `view_summarize`  | needs chat | One-shot file-or-range gist via the chat model. No retrieval.               |

Every tool returns a `status` (`ok` / `no-index` / `no-graph` /
`embedding-service-unreachable` / `chat-service-unreachable` / `error`)
with a human-readable `hint`, so the agent can fall back to grep
instead of pretending success.

### Remote access (`dex mcp --remote`)

`dex serve` exposes the same tools over a bearer-gated REST API so a
client on another host — e.g. a containerized agent — can reuse a hot
host-side index without its own embeddings or GPU. To attach over MCP,
run the stdio shim: it speaks MCP on stdio to the client and proxies
every tool call to the daemon's REST surface.

```bash
# On the host: serve the pre-indexed project(s).
dex serve --addr 127.0.0.1:7920 --project /path/to/repo   # add DEX_SERVE_TOKEN for non-loopback

# In the container/client: attach the shim, bound to a project id.
DEX_SERVE_TOKEN=… dex mcp --remote http://host:7920 --project-id <sha>
```

The project is targeted by its dex **id** (sha256 of the canonical host
root), not by path — so a container's checkout maps onto the host's hot
index. Pass `--project-id` explicitly (a container's checkout path
differs from the host root, so a locally computed id would be wrong);
`--project-root` computes the id locally when the shim runs on the same
host, and if neither is given the shim auto-resolves when the daemon
serves exactly one project. The token comes from `DEX_SERVE_TOKEN`. The
shim holds no index — it's a thin proxy, so the tool surface and
responses are identical to a local `dex mcp`.

## Environment

| Variable          | Default                          | Meaning                                                                      |
| ----------------- | -------------------------------- | ---------------------------------------------------------------------------- |
| `DEX_EMBED_URL`   | `http://127.0.0.1:8082`          | OpenAI-shape `/v1/embeddings` base URL.                                       |
| `DEX_EMBED_MODEL` | `Qwen/Qwen3-Embedding-4B`        | Embedding model name forwarded as `model`.                                    |
| `DEX_INDEX_DIR`   | `~/.cache/dex`                   | Per-project index files.                                                      |
| `DEX_CHAT_URL`    | `http://127.0.0.1:8081`          | `/v1/chat/completions` — `generate`, `view_summarize`, index-time summaries. |
| `DEX_CHAT_MODEL`  | `Qwen/Qwen2.5-Coder-7B-Instruct` | Chat model.                                                                   |

Tuning knobs (rerank, compress, draft, summary, batch sizes, timeouts,
cache toggles) — see [docs/tuning.md](docs/tuning.md).

## How it works

Tree-sitter parses source into named structural chunks (functions,
methods, types, classes). Each chunk hits a self-hosted
`/v1/embeddings` endpoint (ollama, vLLM, or TEI; local or
SSH-tunneled). Embeddings land in a `sqlite-vec` (`vec0`) virtual
table; at query time, vec0 cosine KNN and SQLite FTS5/BM25 are fused
via RRF, with an optional cross-encoder rerank over the fused pool.
Architecture diagram: [docs/architecture.md](docs/architecture.md).
Storage schema, RRF math, vec0 KNN, multi-worktree workflow, embedding
contract, code-gen details: [docs/internals.md](docs/internals.md).

`dex index` also adds a Go-specific structural layer built on
`go/packages` + `go/types` (type-resolved, not regex). The extractor
emits `package` / `file` / `function` / `method` / `type` / `field` /
`import` nodes and `contains` / `imports` / `has_method` / `has_field`
/ `embeds` / `implements` / `calls` edges. Function and method nodes
link back to chunks via `graph_nodes.chunk_id`, so a single SQL join
surfaces graph neighbourhood + source code for any hit. `references`
edges land with the planned LSP integration.

## Comparison

| Capability | dex | grep / ripgrep | cloud RAG / IDE search |
|---|---|---|---|
| Single local binary | ✓ | ✓ | ✗ (service) |
| **Source never leaves the machine** | ✓ self-hosted embeddings | ✓ | ✗ (uploads code) |
| Intent / semantic search | ✓ | ✗ (literal only) | ✓ |
| **Hybrid vector + BM25 fused via RRF** | ✓ | ✗ | partial |
| Cross-encoder rerank | ✓ optional | ✗ | rare |
| Type-resolved call graph (callers/callees) | ✓ Go (`go/types`) | ✗ | partial (IDE) |
| **Intent-routed composed bundle (`ask`)** | ✓ | ✗ | ✗ |
| Inline content — no follow-up Read | ✓ | n/a | partial |
| Auto-reindex on file change | ✓ `dex watch` | n/a | varies |
| MCP-native for AI agents | ✓ | ✗ | varies |

dex isn't trying to replace your editor's search — it ships the
primitives an agent needs (intent routing, hybrid retrieval, a typed
call graph, inline summaries) over MCP, while keeping every byte of
your code on your own machine.

## Docker

```bash
docker build -t dex .
docker run --rm -v "$PWD":/work:ro -v dex-cache:/cache \
    -e DEX_EMBED_URL=http://host.docker.internal:8082 \
    dex index /work
```

Tree-sitter needs CGO, so the build stage uses Alpine's musl toolchain
to produce a static binary on `distroless/static` (final image ~36 MB,
no shell). For a host-bound `/cache`, add `--user "$(id -u):$(id -g)"`
(the image runs as distroless `nonroot`, uid 65532).

## What gets indexed

Indexing is **opt-in**. dex indexes nothing until a project declares an
allow-list in `.dex/config.toml`:

```toml
[index]
include = ["cmd/", "internal/", "*.md"]  # gitignore grammar; file-level allow-list
ignore  = ["testdata/"]                  # appended to the exclude set below
```

With no config (or an empty `include`), the index stays empty — `dex
index`/`watch` print a warning so it isn't a silent no-op. `include`
gates files only; the walk still descends every non-excluded directory,
so `*.md` matches at any depth. The exclude rules below always apply on
top.

## Ignore rules

`.gitignore` is respected. A built-in `.dexignore` skips `.env*`,
`*.pem`, `*.key`, `id_rsa*`, `id_ed25519*`, `secrets.yml`, `*.tfvars`,
`.terraform/`, `node_modules/`, `vendor/`, `.venv/`, `__pycache__/`,
`target/`, `dist/`, `build/`. It also skips generated / aggregated
artifacts that would otherwise pass the extension allow-list: `*.d.ts`,
`llms.txt` / `llms-full.txt`, JS lockfiles (`package-lock.json`,
`pnpm-lock.yaml`, `npm-shrinkwrap.json`), and `coverage/`,
`.nyc_output/`, `htmlcov/`, `__snapshots__/`. Files matching common
secret patterns in their first 4 KB are skipped at index time.

## Documentation

- [Architecture](docs/architecture.md) — diagram + data flow
- [Internals](docs/internals.md) — storage schema, RRF math, vec0 KNN, worktrees
- [Tuning](docs/tuning.md) — rerank, summary, batch, timeout, cache knobs
- [`docs/claude-md-snippet.md`](docs/claude-md-snippet.md) — drop-in CLAUDE.md routing block

## License

MIT — see [LICENSE](./LICENSE).
