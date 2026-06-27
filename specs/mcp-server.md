---
id: mcp-server
status: living
last_verified: 621894f
owners: [aleh]
covers:
  - "internal/mcp/server.go"
  - "internal/mcp/context.go"
  - "internal/mcp/answer.go"
  - "internal/mcp/server_graph.go"
  - "internal/compress/noncode_map.go"
  - "internal/mcp/server_knowledge.go"
  - "internal/mcp/server_session.go"
  - "internal/mcp/server_grep.go"
  - "internal/mcp/server_tree.go"
  - "internal/mcp/server_shell.go"
  - "internal/mcp/server_smells.go"
  - "internal/mcp/server_routes.go"
  - "internal/mcp/http_mcp.go"
  - "internal/mcp/remote.go"
---
# MCP Server (stdio)

## Intent

`dex mcp` is dex's primary interface: a Model Context Protocol server spoken over
stdio that lets Claude Code reach the local semantic index as native tools. It
leads with a single `ask` tool that routes a free-text question across semantic
search, symbol lookup, and graph expansion and (when a chat model is wired)
synthesizes a cited answer — so an agent reaches for one dex tool before fanning
out with Grep/Glob/Read. Every tool is scoped to one project, resolved from the
caller's working directory, and every response carries a structured `status` so
that when the index is missing or a backend is offline the agent gets an explicit
fallback signal rather than a hard failure. This spec covers the stdio MCP tool
interface and its contract; the same handlers re-exposed as REST endpoints for
service clients are the http-api spec's.

## Behavior

- WHEN `dex mcp` starts, dex registers an MCP server (name `dex`) on stdio and
  blocks serving JSON-RPC until the transport closes or the context is cancelled.
- WHERE the tool surface is defined, a single `registerTools` path wires every
  tool onto the server. The local on-disk index (`*Server`), the remote stdio
  shim (`*remoteClient`, proxying to a `dex serve` REST endpoint), and the native
  HTTP-MCP transport all call it, so the surfaces can never drift in tool names,
  JSON schemas, or descriptions — the schema for each tool is derived by the SDK
  from one shared Input type.
- WHERE the surface is capability-derived (#283/#290) rather than tiered, tools
  are registered only when the backend they need is available:
  - The always-on lane (registered even with no embedder or chat model) is
    `ask`, `grep`, `ls`, and `shell`.
  - The default verb lane (non-weak model) adds `map`, `trace` (incl.
    `--dir impact`), `locate`, `review`, `refactor`, `verify`, `check`, `read`, and `notes` — the everyday
    navigation + reading verbs (`locate` for one-call orientation around a code
    location, `review` for per-hunk PR intelligence) plus persistent project
    memory (#548) and `refactor` for type-precise edit planning. `locate` and
    `review` are pure composition over the index and need no chat model; their
    callers lane degrades to empty without a graph. `locate` also carries a batch
    `claims` mode (#708): given a set of `{ref:'file:line', symbol?}` citations it
    resolves each against the index in one call and returns `results[]` —
    `ok`/`moved` (with the corrected `found_at`)/`gone`/`no_file` — so an agent
    can verify locations carried from notes or memory still hold before citing
    them, without N defensive reads. `refactor` needs no index at
    all — it loads source on-demand via go/packages and returns byte-exact edit
    triples (read-only #551: it never writes; the agent applies the edits).
    `verify` (#686, epic #683) is the one NON-read-only default verb: it runs the
    tests a change implicates (working-tree diff / `ref` range / `symbol`
    blast-radius → Go packages) through the shell pipeline, so a failing run
    stages a `gotcha_candidate` — closing change → verify → learn. Go-only in v1;
    `check` (#708) is read-only: batch ref-verification of `file:line[:symbol]`
    claims against the index — returns `ok|moved|gone|no_file|parse_error` per
    claim, with `found_at` when a symbol has moved within the same file. Use it
    after code edits to confirm cited locations are still valid.
    the command template is `command` / `$DEX_VERIFY_CMD` overridable.
    `notes` needs no embedder or chat model,
    and the read path (facts auto-injected into `ask`) is inert if the agent
    can never write, so the write verb headlines the default surface.
  - `find` (semantic search) is registered only WHEN a query-time embedder is
    wired (`embedAvailable`); with `DEX_EMBED_ENGINE=none` (the lean profile) it
    is omitted and retrieval degrades to BM25 (`grep`) + symbol + graph + file
    lanes — `ask` stays and routes to the non-semantic lanes.
  - `read` is always registered (its structural modes — `full` raw content,
    `signatures`, `skeleton`, `map`, `lines:N-M`, and `analyze` (a per-mode
    token-cost comparison + recommended mode + a whole-file expansion `handle`
    for lazy selective reads, no file content, #623/#620) — need no
    chat model). Only `read mode=summary` (the LLM digest) needs a chat model;
    when none is wired it returns `status=needs-chat` rather than being hidden.
  - The power lane (`deps`, `diff`, `clusters`, `routes`, `smells`,
    `cohort`, `status`, `budget`, `session`, `checkpoint`) is gated behind
    `DEX_EXPERT` — the default verbs above cover everyday work, so the stdio
    surface stays small unless an operator opts into the full set. (Call-graph
    queries are NOT standalone tools: `callers`/`callees`/`path` are reached via
    `trace --dir`, #575.)
  - WHEN a weak/local model is detected, the full surface is hidden and only the
    always-on lane (`ask`, `grep`, `ls`, `shell`) is exposed.
  This yields a flat, prefix-free surface: the default
  `ask`, `find`, `map`, `trace`, `locate`, `review`, `refactor`, `verify`, `check`, `read`, `grep`,
  `ls`, `shell`, `notes` plus the `DEX_EXPERT` power lane `deps`, `diff`,
  `clusters`, `routes`, `smells`, `cohort`, `status`, `budget`, `session`,
  `checkpoint`.
- WHEN a chat model is configured, `ask` returns a synthesized, citation-bearing
  (`path:line`) prose answer grounded in the evidence bundle; WHEN the chat leg
  is unreachable the answer is omitted and the caller falls back to the evidence
  bundle plus the `next_action` directive.
- WHEN `ask` runs, it infers intent
  (behavior_search/symbol_lookup/callers/callees/architecture/package_topology/editing_context),
  composes the matching lanes, and returns a compact bundle (semantic hits,
  symbols, suggested reads with inlined contents, a `next_action`, an `avoid`
  line); a caller MAY override the inferred intent. The explicit-only `assemble`
  intent (#687, never auto-routed) turns `ask` into a context-assembler: it
  inlines symbol bodies in submodular keyword-coverage order — the non-redundant
  subset covering the most of the query per byte ((1 − 1/e) greedy) — under the
  denser exploration byte budget, and suppresses prose synthesis so the
  structured working set IS the answer. Inline content shares one
  per-intent byte pool across both lanes; oversize ranges arrive `truncated:true`
  with their original line range, and `no_inline:true` omits payloads.
- WHERE intent is `assemble`, the bundle also carries a `concerns` completeness
  signal (#725): the query's coverage keywords split into `covered` (a symbol
  whose body was inlined is about the concern — same name+signature haystack
  the submodular selector scored) and `dropped` (the byte budget left no symbol
  body about the concern). WHEN any concern is dropped, `next_action` states the
  set is partial — an honest partial beats a false floor. WHEN the partial set
  still holds an inlined anchor, that caveat upgrades to a concrete chained
  directive (#729) — "trace callees of `<anchor>` (or raise k) to pull the
  dropped concerns in" — handing the caller the next graph move rather than
  only flagging incompleteness; with no inlined anchor (the pure-prose miss,
  nsyms=0) it stays generic, since there is nothing in the set to chain from.
- WHERE intent routes to `editing_context` (edit/modify/refactor/extend
  phrasing) AND the result spans more than one site, `next_action` nudges the
  caller toward `intent=assemble` (#725) — so the "batch reads" instinct reaches
  the working-set assembler without the caller knowing the knob exists. The
  routing itself is unchanged; the nudge is additive.
- WHEN `ask` is called with an empty question, it routes to session-start
  orientation (intent `orient`): a deterministic, zero-inference L0+L1 codemap
  bundle (repo cluster overview, an L1 zoom into the most-central cluster, an
  "entrypoints" section — the project's main() functions, where execution starts;
  a "layers" section — internal packages topo-sorted into dependency layers
  (foundational → top), the top-down architecture spine; and an "external
  dependencies by capability" section — third-party/stdlib imports bucketed into
  database/network/gpu-ml/serialization/crypto/process/cloud, #581) is returned
  in the `map` field, so an agent names the right package before any
  `find`. The bundle is byte-stable across calls (cache-friendly) and is rendered
  through the same path as `dex orient` and `dex map`. WHEN no call graph is
  indexed it degrades to a `no-graph`/`no-index` hint pointing at
  `dex index . --graph=only`, never an error.
- WHEN `find` is called, dex embeds the query and returns top-k chunks; identifier
  tokens in the query are also looked up by exact symbol name and fused via RRF.
  Supports `languages`, `path_glob`, and `exclude` filters.
- WHERE exact identifier lookup is needed, `find` fuses exact symbol-name hits
  via RRF (and `ask` detects identifiers and runs the same lookup), so there is
  no standalone `lookup` MCP tool (#685). The fast no-embedding SQL symbol lookup
  still backs `find`'s fusion, `locate`'s resolver, and the REST `/lookup` route.
- WHEN a graph tool is called (`trace` with `--dir callers|callees|path|impact`,
  `deps`, `diff`, `clusters`, `routes`, `smells`), dex reads the static
  call/import graph (no embedding, no chat) and returns `no-graph` when the
  relevant edges have not been indexed (`dex index . --graph=only`). `trace`
  accepts a bare name, a qualified method (`(*Server).RunStdio`), or a
  package-qualified name and returns multiple matches for disambiguation. For a Go
  method that implements a project interface, the call graph follows interface
  DISPATCH (#604): `--dir callers` additionally returns the call sites that
  reach it through the interface method, each tagged with `via`; and `--dir impact`
  (transitive blast-radius BFS) + its risk tier count those dispatch-reached
  callers too — so dynamic dispatch isn't missed. A pure graph traversal over the
  existing `implements` edges + interface-method nodes (no go/types at query time).
  `--dir impact` also returns `tests_to_run`: the sibling tests (foo.go ↔ foo_test.go)
  of the blast-radius files — the target's own test plus the shown callers' — so
  the change→verify loop ("you edited X; run these") is one call (#654). impact
  folded from a standalone tool into `trace --dir impact` (#684).
- WHEN `read` reads or summarizes a file path, the path must resolve inside the
  project root; a path escaping the root is rejected so an MCP caller can't read
  arbitrary files. Files over 64 KB are truncated. `paths[]` (max 10) reads
  several files in one call under the same mode with `## path` section headers,
  and the per-path traversal check applies to every entry. Responses carry an
  `etag` (content hash): on re-read pass it back to get `status:unchanged` (reuse
  context) or `status:delta` (a compact unified diff). `mode=skeleton` emits
  exported type declarations in full plus signatures with `@B<n>` body handles
  expandable on demand. Passing `task` routes compression by intent. `ref` (a git
  revision) time-travels the read to that commit — `full` (raw) or `signatures`
  (the historical API, tree-sitter-compressed off the git content, not the HEAD
  index); index-backed modes are rejected and the file must still exist now
  (#644/#657). Any note whose `scope` binds the file is returned in `scoped_notes`
  (gotcha-on-touch, #645/#650), surfaced uniformly across every read mode — read
  it before editing.
- WHEN `read mode=map` is called on a non-code file (Markdown, JSON, YAML, TOML,
  lock files), dex returns a structural outline — heading tree, JSON key
  hierarchy (depth ≤ 3), YAML/TOML sections, or lock-file dependency counts —
  without using the index or a chat model; code files fall through to the symbol-
  map path.
- WHEN `notes` is called, dex manages persistent project knowledge that survives
  session resets: `action=add` (store a fact with an archetype and confidence;
  the response's `similar` list warns when a near-duplicate note already exists
  — Jaccard word-overlap ≥ 0.5, the write-time companion to the GC merge pass —
  so the author can `delete` the superseded one, #606; an optional `scope` binds
  the fact to a file glob / path / package — `internal/mcp/*_test.go`,
  `internal/store` — so `locate` surfaces it proactively when it touches a
  matching file, "gotcha-on-touch", #645; when `scope` is omitted but the body
  names a real project file/glob, the add response's `scope_suggestion` proposes
  one so the binding is discoverable, #658),
  `action=list` (top-k by salience; pass `scope=<path>` to instead return the
  notes whose scope binds that path — what would surface on touching it, #653),
  `action=delete` (by id). Archetypes:
  Architecture | Gotcha | Convention | Decision | Observation | Dependency |
  Pattern | Fact. High-salience facts are injected into `ask` responses as
  `knowledge_facts`. No embedding required.
- WHEN `session` is called, dex manages per-project session memory across tool
  calls: `set_task`, `add_note`, `add_file`, `get`, `clear`, `snapshot` (recovery
  block after compaction), `budget` (context-window utilization estimate),
  `heatmap` (per-file access frequency and compression savings), `export`
  (serialise task + working-set files + notes into a `dex-session-v1` JSON bundle
  — paths + content etags only, never file content, so it is safe to commit for
  team handoff), `import` (restore a bundle into a fresh session and return a
  recovery digest; etags drive staleness detection so files changed or vanished
  since export are flagged for re-read first — import never pre-warms the read
  cache, so the first read of a restored file still delivers full content, #603).
  The task + notes + files are surfaced in `ask` responses as `session_task`. No
  embedding required.
- WHEN `checkpoint` is called (DEX_EXPERT), dex keeps a private SHADOW git history
  of the working tree under its cache dir (`<cache>/shadow`), fully isolated from
  the user's `.git` via a GIT_*-scrubbed env + explicit `GIT_DIR`/`GIT_WORK_TREE`
  (the #341 isolation contract): `snapshot` commits the current tree (idempotent —
  no commit when unchanged), `log` lists checkpoints newest-first, `diff` returns a
  byte-capped unified diff between two checkpoints (default `HEAD~1..HEAD`). dex
  only READS the user's tree — there is no restore action; the user's repository is
  never read or mutated (#551, #608).
- WHEN `shell` is called, dex executes a command and returns compressed output
  (the same pipeline as the indexer's log compression). Output is routed by a
  tiered policy — passthrough (dev servers / auth flows, untouched) · verbatim
  (structured queries, ANSI-strip + hard-cap only) · minimal (#616: git
  diff/log/show/blame and dependency audits — drop git index-hash plumbing,
  collapse blank runs, dedup non-signal lines, but keep every diff/error/count
  line) · compress (build/test/lint, full pattern pass). File-write redirects
  (`>`, `>>`) and `tee` are blocked by default — the caller must use the Write
  tool — unless `DEX_SHELL_ALLOW_WRITES=1` opts out (#596); `raw:true` skips
  compression; timeout 60 s. WHEN a command exits non-zero and its output
  matches a known failure signature (build, test, panic, permission, network,
  …), the response carries a low-confidence `gotcha_candidate` (archetype
  `Gotcha`) the agent can confirm via `notes action=add` (#601); the scan is
  pure regex over the already-compressed output, gated on the non-zero exit, and
  omitted otherwise.
- WHEN `grep` is called, dex runs an RE2 pattern search over indexed project files
  (inheriting the project's ignore rules), falling back to a filesystem walk that
  skips `.git`, `vendor`, and `node_modules` when no index exists. Returns up to
  `max_results` (default 50) matches and `status:"no-matches"` when nothing hits.
- WHEN `ls` is called, dex lists indexed files under a directory, showing
  individual files within `depth` levels (default 3) and aggregating deeper files
  into their parent dirs with summed chunk counts. No embedding required.
- WHERE MCP annotations are set, all read-only tools carry `readOnlyHint: true`
  so hosts (e.g. Claude Code plan mode) can skip approval prompts for them.
- WHEN any tool resolves a project, it canonicalizes the caller's `project_root`
  (defaulting to the server's working directory) to a single per-project index,
  so every tool reads the index for exactly the repo the agent is working in.
- IF no `project_root` is given and the working directory can't be determined, a
  tool returns `status:"error"` with a hint to pass `project_root` explicitly.
- IF the resolved project has no index on disk, a tool returns `status:"no-index"`
  with a hint to run `dex index <root>` and fall back to grep meanwhile.
- IF the embedding service is unreachable, a search/ask tool returns
  `status:"embedding-service-unreachable"` with the endpoint and a hint to fall
  back to grep/Glob/ripgrep — never a partial or empty result masquerading as ok.
- IF the chat service is unreachable, `read` returns
  `status:"chat-service-unreachable"`; graph tools that lack indexed edges return
  `status:"no-graph"`; symbol lookup with no match returns `status:"not-found"`,
  surfacing near-miss candidates when any exist.
- WHILE the index is older than 24h, a search response is flagged `stale:true`
  with a hint to re-index, but still answers.
- WHILE the stdio session is live and auto-watch is enabled, the first request
  that resolves a project lazily spawns one per-project file watcher (at most once
  per project per session) that keeps both the chunk and graph lanes fresh on
  change (its `AfterIndex` hook re-runs the graph phase and embeds new nodes); the
  watcher shares the session context and stops on shutdown.
- WHERE a handler panics, an `addTool` recovery guard converts it to a structured
  tool error instead of crashing the MCP session.

## Non-goals

- **The REST transport.** The same handlers re-exposed as HTTP/REST endpoints
  (`dex serve`, project-id-keyed URLs, bearer auth) are the **http-api** spec.
  This spec is the stdio MCP tool interface.
- **What the lanes compute.** Ranking and hybrid fusion are **semantic-search**;
  exact identifier lookup is **symbol-search**; call/import edges are **graph**;
  answer synthesis strategy detail is **ask**; vector storage is **storage**.
  Here only the tool surface, scoping, and status contract.
- **Building the index.** Walking, chunking, and embedding a repo are **indexing**;
  this server only reads an already-built index (and triggers re-index via watch).
- **The watch daemon internals.** Debounce and the `AfterIndex` graph refresh are
  **watch**; here only that a session lazily spawns one and stops it on shutdown.
- **Remote MCP access.** Reaching the hot host index from a container goes through
  the stdio→REST shim (`dex mcp --remote`, `remote.go`) and the native HTTP-MCP
  transport at `/v1/projects/{id}/mcp` (`http_mcp.go`); both register the same
  surface via `registerTools`, so the tool contract here applies unchanged.
- **Mutating the codebase.** dex is read-only by design (#551): every tool
  carries `readOnlyHint: true` and there is no edit/write/apply verb. Editing is
  the host agent's native job (Claude Code's `Edit`/`Write`), and a read-only
  surface composes cleanly with it — dex *locates and explains* (find, trace,
  impact, read), the agent *changes*. This is a deliberate boundary, not a gap:
  a symbol-level edit tool (cf. Serena's `apply`-at-symbol over LSP) was
  considered and declined to keep the surface small and the failure modes
  read-only. `notes` is the sole persistence verb, and it writes dex's own
  knowledge store, never project files.

## Checklist

- [x] `dex mcp` registers an MCP server on stdio, blocks until transport closes
- [x] `ask` leads the surface; composes lanes + synthesizes cited answer when a chat model is wired
- [x] `ask` with an empty question → session-start orientation (intent `orient`): deterministic L0+L1 codemap in `map`, byte-stable, single-sourced with `dex orient`/`dex map`; degrades to a `no-graph`/`no-index` hint, never an error (#348 / #316 story 6)
- [x] Single `registerTools` path wires the surface for stdio, remote shim, and HTTP-MCP — no name/schema drift
- [x] Capability-derived exposure (#283/#290): `find` gated on `embedAvailable`; `read` always registered (structural modes need no chat; `mode=summary` returns `needs-chat` when no chat model); power lane gated on `DEX_EXPERT`; weak model → only `ask`/`grep`/`ls`/`shell`
- [x] Flat, prefix-free surface of up to 21 tools (#427): default `ask`/`find`/`map`/`trace` (incl. `--dir impact`, #684)/`read`/`grep`/`ls`/`shell`/`notes` + `DEX_EXPERT` power lane (no `DEX_TOOLS` tiers)
- [x] `read mode=map` returns a structural outline for non-code files (Markdown/JSON/YAML/TOML/lock); no LLM, no index
- [x] `read paths[]` batch: max 10 files, same mode, concatenated `## path` output; path-traversal check per entry; etag/delta/skeleton modes
- [x] `read` path traversal rejected (must resolve inside project root); files over 64 KB truncated
- [x] `notes` actions add/list/delete; high-salience facts injected into `ask` as `knowledge_facts`
- [x] `session` actions set_task/add_note/add_file/get/clear/snapshot/budget/heatmap; surfaced in `ask` as `session_task`
- [x] `shell` blocks `>`/`>>`/`tee` (opt out: `DEX_SHELL_ALLOW_WRITES=1`); `raw:true` skip; 60 s timeout; compressed output
- [x] `grep` RE2 search over indexed files; fs-walk fallback; `max_results` cap (default 50); `no-matches` status
- [x] Read-only tools carry `readOnlyHint: true` MCP annotation
- [x] Read-only by design (#551): no edit/write/apply verb; editing is the host agent's job, dex locates/explains. Deliberate boundary, not a gap.
- [x] Per-project scoping: `project_root` → canonical index (cwd default)
- [x] Structured statuses: `no-index`, `embedding-service-unreachable`, `chat-service-unreachable`, `no-graph`, `not-found`, `stale`
- [x] Lazy per-project auto-watcher spawned per session, refreshes chunk + graph lanes, stops on shutdown
- [x] Handler panics converted to structured tool errors by the `addTool` recovery guard
- [x] Verified against the code by the verify workflow (flip to `living`)
