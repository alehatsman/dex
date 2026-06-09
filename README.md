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
mooncake task install   # → ~/bin, no sudo; atomic rename-swap, so
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
                                           #   --max-content-bytes, -v
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
dex index <path>           # build or refresh (chunks + Go graph + git commits)
                           #   --graph=off     skip graph phase
                           #   --graph=only    refresh just the graph
                           #   --dry-run       preview what would be indexed
                           #   --no-git        skip git commit indexing phase
dex index summarize <path> # drain pending_summaries queue
dex generate <path> "..."  # RAG: top-k chunks → chat endpoint
dex guide <path>           # render LLM_GUIDE.md from repo+pkg summaries
                           #   --check exits 1 when guide is out of date
dex watch <path>           # fsnotify-driven auto-reindex
dex clone <src> <dst>      # seed a worktree's index from a sibling
dex reindex <path>         # drop and re-embed from scratch
dex nuke <path>            # delete the on-disk index
dex compact <path>         # concatenate all indexable files to stdout
                           #   --out FILE, --max-bytes N, --strip
dex mcp                    # MCP server over stdio

# config / setup
dex setup                  # guided first-run wizard (check + index + MCP rules)
                           #   --check exits 1 when setup is incomplete
dex config init            # scaffold .dex/config.yml with commented defaults
                           #   --force overwrite; --full include all knobs
dex doctor                 # check setup end-to-end (endpoints, index, MCP wiring)
dex env [--all] [--doc]    # print effective DEX_* config with sources
dex completion bash|zsh|fish  # emit shell tab-completion script
dex version                # print build version

# Claude Code hooks (read JSON on stdin; see "Claude Code hooks" below)
dex hook inject            # UserPromptSubmit  → inject ask context per prompt
dex hook rewrite           # PreToolUse(Bash)  → rewrite rg/grep to dex
dex hook redirect          # PreToolUse(Read)  → signatures view for big files
dex hook observe           # PostToolUse/Stop  → append event to hooks.jsonl
```

`dex env` prints effective config with sources (`env|file|default`); `dex -h`
lists everything.

Rather than threading the `DEX_*` knobs through the MCP env block, the systemd
unit, and your shell, pin them once in `.dex/config.yml` (read from the working
dir at startup). Precedence is **env var > config file > default**, so any env
override still wins:

```yaml
endpoints:
  embed: http://localhost:11434   # DEX_EMBED_URL
  chat:  http://localhost:11434   # DEX_CHAT_URL
models:
  embed: mxbai-embed-large        # DEX_EMBED_MODEL
  chat:  qwen2.5-coder:14b        # DEX_CHAT_MODEL
tools:
  tier: power                     # DEX_TOOLS
env:                              # escape hatch: any DEX_* knob verbatim
  DEX_EMBED_CONCURRENCY: 8
```

`DEX_SERVE_TOKEN` is a secret and is never read from the file — keep it in the
environment. (Indexing include/ignore globs live in the same file under the
`index:` section — see "What gets indexed" below.)

### Context profiles

`DEX_PROFILE=<name>` activates a named context profile that adjusts defaults
per task type — no per-call flag overrides needed. Four built-ins:

| Profile   | target_model | file_view mode | compression | max files (k) |
| --------- | ------------ | -------------- | ----------- | ------------- |
| `claude`  | claude       | full           | tight       | 10            |
| `explore` | *(default)*  | full           | normal      | 12            |
| `bugfix`  | *(default)*  | full           | tight       | 8             |
| `ci`      | *(default)*  | signatures     | minimal     | 5             |

The `claude` profile is the recommended default for Claude Code users — it
selects the `cl100k_base` tokenizer (within ~3% of Claude's real tokeniser) so
every "saved N tokens" report is honest rather than a GPT-4o approximation.

Custom profiles live in `.dex/profiles/<name>.yml` (project-local) or
`~/.dex/profiles/<name>.yml` (global). Format:

```yaml
target_model: claude          # selects tokenizer family + aggressiveness defaults
read:
  default_mode: full          # full | signatures | map
compression:
  output_density: tight       # normal | tight | minimal
budget:
  max_files: 8                # cap on k for search_semantic
  context_fraction: 0.6       # fraction of context window one response may use
```

`target_model` accepts any substring recognised by the tokenizer detector:
`claude`/`anthropic`/`sonnet`/`opus`/`haiku` → `cl100k_base`;
`gemini`/`google` → Gemini (o200k × 1.08);
`llama`/`deepseek`/`qwen`/`mistral` → `cl100k_base`;
anything else → `o200k_base` (default).

### Workspace search

`search_workspace` searches across multiple projects in one call. Declare the
project set in `.dex/workspace.yml` at any project root:

```yaml
projects:
  - path: /home/user/code/api-server
    label: api                  # optional; defaults to directory name
  - path: ../frontend           # relative paths resolved from workspace.yml dir
```

Each project must be independently indexed. `search_workspace` embeds the query
once, runs hybrid search per project, and merges with RRF — tagging every hit
`[project:label]` so you know which project it came from.

### Session SLO monitoring

dex can warn, throttle, or block when a session exceeds resource thresholds.
Configure under `slo:` in `.dex/config.yml`:

```yaml
slo:
  - name: context budget
    metric: context_tokens      # context_tokens | tool_calls | shell_calls
    threshold: 80000
    action: warn                # warn | throttle | block
    percent: 80                 # fire an early warning at 80% of threshold
  - name: shell cap
    metric: shell_calls
    threshold: 50
    action: throttle
```

Actions: `warn` annotates the next tool response; `throttle` downgrades
`file_view` from `full` to `signatures`; `block` returns an error. A 30 s
debounce window prevents annotation spam. Thresholds are per-session and reset
on reconnect.

When running as `dex mcp`, the server registers tools in three tiers controlled
by `DEX_TOOLS=ask|standard|power` (default `standard`):

| Tool                 | Tier      | What it does                                                                |
| -------------------- | --------- | --------------------------------------------------------------------------- |
| `ask`                | ask+      | **Primary entry point.** Router + composed bundle.                          |
| `ctx_overview`       | standard+ | Task-relevant project map: top-k files ranked by semantic similarity + graph centrality. |
| `ctx_session`        | standard+ | Declare task / record notes / track files across tool calls. Surfaces as `session_task` in `ask`. |
| `ctx_knowledge`      | standard+ | Store / recall / consolidate cross-session facts; revision tracking on re-add. |
| `ctx_agent`          | standard+ | Multi-agent coordination bus: `announce`/`post`/`read`/`list` — share findings across concurrent agents. |
| `ctx_nav`            | standard+ | Return the dex tool-routing guide — which tools exist, their tier, and when to call each. |
| `ctx_feedback`       | standard+ | Report output-ratio feedback to the adaptive compression policy (intent + ratio + last read mode). |
| `ctx_prefetch`       | standard+ | Given `changed_files[]`, walks the call/import graph via spreading activation and prefetches the most structurally-related neighbors — inlines content so no follow-up reads are needed. Requires graph index. |
| `ctx_shell`          | standard+ | Execute a shell command and return compressed output (strips build noise, deduplicates logs). |
| `search_context`     | standard+ | Embed query → top-K file signatures + best symbol body in one round-trip.  |
| `search_workspace`   | standard+ | Search across all projects in `.dex/workspace.yml` — runs hybrid search per project and merges with RRF, tagging each hit `[project:label]`. |
| `search_grep`        | standard+ | Regex search over project files (no embedding). For exact-match queries: cross-cutting references, import paths, string literals. |
| `file_tree`          | standard+ | Filesystem subtree with file sizes and extension breakdown.                 |
| `file_view`          | standard+ | Signatures / structural map / LLM gist / line slice. `paths[]` (max 10) for batch; `etag` for change-detection. |
| `search_semantic`    | power     | Hybrid (cosine + BM25 + optional rerank) top-k chunks. Supports `exclude`, `languages`, `path_glob`. |
| `search_symbol`      | power     | Exact identifier lookup (SQL scan, no embedding).                           |
| `search_similar`     | power     | Find chunks semantically similar to code at a given `file:line` — full hybrid pipeline, stronger than `graph_neighbors`. |
| `graph_neighbors`    | power     | Vector neighbours of a known chunk at `path:start_line`.                    |
| `graph_deps`         | power     | `imports` edges for a file or package. Sourced from the static graph.       |
| `graph_callers`      | power     | Incoming `calls` edges (Go-only today).                                     |
| `graph_callees`      | power     | Outgoing `calls` edges (Go-only today).                                     |
| `graph_impact`       | power     | Transitive blast-radius analysis over incoming `calls` with PageRank scoring. |
| `graph_links`        | power     | Outgoing markdown `links`/`wikilinks` — docs a doc points to.               |
| `graph_backlinks`    | power     | Incoming markdown `links`/`wikilinks` — what links to a doc (Obsidian-style).|
| `graph_tags`         | power     | Tag graph: `tag`→documents (ranked) or `doc`→tags. Tag-based clustering.    |
| `graph_routes`       | power     | All reachable call paths between two symbols.                               |
| `graph_smells`       | power     | AST-based code-quality signals: long functions, dead exports, god files. No LLM required. |
| `compress_output`    | power     | Compress raw shell output: strips spinners/noise, deduplicates, summarises go test/git/npm/cargo output. |
| `status`             | power     | Endpoint health (embed / chat / rerank) + indexed projects.                 |
| `spec_check`         | power     | Verify a spec file's checklist against the live index.                      |

`file_view mode=map` returns a structural outline for non-code files
(Markdown heading tree, JSON key hierarchy, YAML/TOML sections, lock-file dep
counts) without touching the index or a chat model. When the current session
has a declared task (`ctx_session`), `mode=signatures` and `mode=map` append the
body of the symbol whose name best matches the task — no follow-up lines: call.
Pass `paths[]` (max 10) to read multiple files in one round-trip; each file is
returned as a `## path` section in the combined output.

`knowledge action=consolidate` (requires `DEX_CHAT_URL`) reads the current
session notes, asks the chat model to extract factual findings, and stores each
one — turning ad-hoc session state into durable facts automatically.
Re-storing a known fact increments its `revision_count`; responses show
"Confirmed (revision N)." so the agent knows whether a fact is new or repeated.

`ctx_overview` returns `status:"partial"` with project markers, a depth-2 filesystem
tree, and top knowledge facts when the index is empty (indexing in progress), so
an agent can orient before the first `dex index` completes.

Every tool returns a `status` (`ok` / `no-index` / `no-graph` /
`embedding-service-unreachable` / `chat-service-unreachable` / `error`)
with a human-readable `hint`, so the agent can fall back to grep
instead of pretending success.

## Claude Code hooks

Beyond the MCP tools, dex can wire itself into Claude Code's
[hook events](https://docs.claude.com/en/docs/claude-code/hooks) so the
index works for the agent **without** it having to remember to call a
tool. Each hook is a `dex hook <sub>` subcommand that reads the hook's
JSON payload on stdin and emits the host's hook-output JSON on stdout.
All four fail open — any error, timeout (3 s on stdin), or missing index
passes the original action through untouched, so a hook never blocks or
breaks a turn.

| Hook (event)                | What it does                                                                                                  |
| --------------------------- | ------------------------------------------------------------------------------------------------------------- |
| `inject` (UserPromptSubmit) | Runs `ask` on the prompt and prepends `additionalContext` — suggested reads + symbols + a `next_action`. Skips prompts under 4 words; 10 s budget. |
| `rewrite` (PreToolUse/Bash) | `rg PATTERN [PATH]` → `dex search semantic`; simple `grep -r …` gets piped through `dex compress-stdin`. Anything with pipes/redirs/unknown flags passes through. |
| `redirect` (PreToolUse/Read)| For indexed code files >400 lines, redirects the Read to a **signatures view** (imports + top-level declarations, bodies dropped) built from the graph — ~97% fewer lines, head order kept. Small/unindexed/non-code files pass through. |
| `observe` (PostToolUse, Stop, PreCompact) | Appends a compact `{ts, tool_name, tokens}` record to `$XDG_DATA_HOME/dex/hooks.jsonl` for session awareness. Fire-and-forget, no output. |

The wiring lives in two committed files so a checkout is ready to go:

- **`.claude/settings.json`** — the hook→command map Claude Code reads on
  open. Active automatically; `mooncake task setup-hooks` also installs
  the git pre-commit/pre-push gates alongside it.
- **`.claude-plugin/manifest.json`** — a plugin descriptor (MCP command +
  hook map + capabilities) for installing dex as a Claude Code plugin.

The `redirect` and `inject` hooks need the project indexed (`dex index .`);
`rewrite` and `observe` work without one. Set `DEX_INDEX_DIR` if your
cache lives off the default path.

### Remote access

`dex serve` lets a client on another host — e.g. a containerized agent —
reuse a hot host-side index without its own embeddings or GPU. The
project is targeted by its dex **id** (sha256 of the canonical host
root), not by path, so a container's checkout maps onto the host's hot
index. There are two ways to attach, both bearer-gated by
`DEX_SERVE_TOKEN`:

```bash
# On the host: serve the pre-indexed project(s).
dex serve --addr 127.0.0.1:7920 --project /path/to/repo   # add DEX_SERVE_TOKEN for non-loopback
```

**Native HTTP-MCP (preferred).** `dex serve` speaks the streamable-HTTP
MCP transport directly at `/v1/projects/{id}/mcp` — attach with no shim
process:

```jsonc
// .mcp.json — one server per project id
{ "dex": { "type": "http",
           "url": "http://host:7920/v1/projects/<sha>/mcp",
           "headers": { "Authorization": "Bearer <DEX_SERVE_TOKEN>" } } }
```

**Stdio shim (`dex mcp --remote`).** For clients that only attach MCP
over stdio, run a thin proxy that speaks MCP on stdio and forwards every
tool call to the daemon's REST surface:

```bash
DEX_SERVE_TOKEN=… dex mcp --remote http://host:7920 --project-id <sha>
```

Pass `--project-id` explicitly (a container's checkout path differs from
the host root, so a locally computed id would be wrong); `--project-root`
computes the id locally when the shim runs on the same host, and if
neither is given the shim auto-resolves when the daemon serves exactly
one project.

Either way the tool surface and responses are identical to a local
`dex mcp` — the same project scoping and bearer auth apply.
`DEX_TOOLS=power` (or the legacy alias `DEX_EXPOSE_RAW_TOOLS=1`) exposes
the full raw surface; `DEX_TOOLS=ask` narrows to just the `ask` tool.

## Environment

| Variable                | Default                          | Meaning                                                                      |
| ----------------------- | -------------------------------- | ---------------------------------------------------------------------------- |
| `DEX_EMBED_URL`         | `http://127.0.0.1:8082`          | OpenAI-shape `/v1/embeddings` base URL.                                       |
| `DEX_EMBED_MODEL`       | `Qwen/Qwen3-Embedding-4B`        | Embedding model name forwarded as `model`.                                    |
| `DEX_INDEX_DIR`         | `~/.cache/dex`                   | Per-project index files.                                                      |
| `DEX_CHAT_URL`          | `http://127.0.0.1:8081`          | `/v1/chat/completions` — `generate`, `file_view`, index-time summaries.     |
| `DEX_CHAT_MODEL`        | `Qwen/Qwen2.5-Coder-7B-Instruct` | Chat model.                                                                   |
| `DEX_PROFILE`           | *(unset)*                        | Named context profile: `claude`, `explore`, `bugfix`, `ci`, or a custom `.dex/profiles/<name>.yml`. |
| `DEX_ATTENTION_LAYOUT`  | *(unset)*                        | Set `1` to sort evidence chunks by structural importance before the chat model sees them (error/panic > import > func > comments). Improves answer quality on complex questions. |
| `DEX_NO_GIT_INDEX`      | *(unset)*                        | Set `1` to skip the git commit indexing phase (equivalent to `dex index --no-git`). |
| `DEX_TOOLS`             | `standard`                       | MCP tool surface: `ask` (single tool), `standard` (default), or `power` (full raw surface). |
| `DEX_NO_AUTO_OLLAMA`    | *(unset)*                        | Set `1` to disable best-effort auto-start of the local ollama daemon.         |

Run `dex env --all --doc` to see the full list of tuning knobs with inline
descriptions (rerank, compress, draft, summary, batch sizes, timeouts, cache
toggles).

## How it works

Tree-sitter parses source into named structural chunks (functions,
methods, types, classes). Each chunk hits a self-hosted
`/v1/embeddings` endpoint (ollama, vLLM, or TEI; local or
SSH-tunneled). Embeddings land in a `sqlite-vec` (`vec0`) virtual
table; at query time, vec0 cosine KNN and SQLite FTS5/BM25 are fused
via RRF, with an optional cross-encoder rerank over the fused pool.

If the embedding endpoint is unreachable, dex **degrades instead of
crashing**: the semantic leg drops out and search runs BM25-only, while
`search_symbol`, the `graph_*` tools, `file_tree`, and `file_view`
(`signatures`/`map`/`lines`) keep working — so `dex mcp`/`dex serve` stay useful
on an already-indexed repo with ollama down. When `DEX_EMBED_URL` is unset and
ollama is installed but not running, dex best-effort starts it (`ollama serve`;
opt out with `DEX_NO_AUTO_OLLAMA=1`).

`dex index` runs in three phases:

1. **Chunk + embed** — tree-sitter parses source into named structural chunks;
   each chunk is embedded and stored in `sqlite-vec`. Incremental: only changed
   files are re-processed (content-SHA gating).

2. **Go graph** — `go/packages` + `go/types` (type-resolved, not regex) extracts
   `package` / `file` / `function` / `method` / `type` / `field` / `import`
   nodes and `contains` / `imports` / `has_method` / `has_field` / `embeds` /
   `implements` / `calls` edges. Function nodes link back to chunks via
   `graph_nodes.chunk_id`. Skip with `--graph=off`; refresh only with
   `--graph=only`.

3. **Git commits** — each commit becomes a searchable chunk
   (`kind=git_commit`, `path=git:<hash>`) containing hash, author, date,
   subject, body, and changed-file list. Incremental via a watermark; force-push
   detection wipes and re-indexes. Skip with `--no-git` or `DEX_NO_GIT_INDEX=1`.

At query time, the semantic pipeline has three ranking layers: vec0 cosine KNN
+ BM25 fused via RRF, an in-RAM multi-scale TF-IDF index that pre-filters
candidates to the most structurally-relevant files/dirs before rerank, and a
spreading-activation graph lane that follows `calls`/`imports` edges from the
top hits to surface closely related code even without keyword overlap. An
optional cross-encoder rerank runs over the fused pool.

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
allow-list in `.dex/config.yml`:

```yaml
index:
  include: ["cmd/", "internal/", "*.md"]  # gitignore grammar; file-level allow-list
  ignore:  ["testdata/"]                  # appended to the exclude set below
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

- [`docs/model-selection.md`](docs/model-selection.md) — embedding, chat, reranker, and summary model recommendations (MTEB benchmarks, VRAM budgets, quant guide)
- [`docs/claude-md-snippet.md`](docs/claude-md-snippet.md) — drop-in CLAUDE.md routing block for agents
- [`docs/observability.md`](docs/observability.md) — log field conventions and subsystem tagging
- [`docs/how-dex-guide-works.md`](docs/how-dex-guide-works.md) — how `dex guide` generates LLM_GUIDE.md
- [`specs/`](specs/) — living specs for indexing, search, graph, MCP server, storage, and more
- `dex env --all --doc` — inline docs for every tuning knob (rerank, batch sizes, timeouts, cache)

## License

MIT — see [LICENSE](./LICENSE).
