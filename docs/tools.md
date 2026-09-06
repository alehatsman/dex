# Tools

This is the **query/tool contract**. Since query-unification (#848: CLI slice
#849, REST slice #851, MCP slice #852) all three transports converge on the
**same shape** — they no longer diverge on purpose the way they briefly did:

- **Over MCP** an agent sees **one verb by default** — `query` (read). That
  set is constant across every deployment profile. dex is retrieval over the
  codebase, not agent memory: the `record` write verb and the L3 knowledge
  subsystem were removed in #205 — durable findings are the harness's
  file-based memory now.
- **On the CLI** there is no separate `dex ask`/`search`/`read`/`locate`/
  `trace`/`review_diff`/`check`/`grep` verb either (#849 deleted all of
  them) — the human surface is `dex query [--kind=K] [--want=W] [path]
  <input...>`, same lane classifier as MCP's `query` tool. CLI form for the
  fixed lifecycle commands (`index`, `setup`, …) stays `dex <verb> [path]
  <args…>` (`path` defaults to cwd).

The two are one engine: `--kind=`/`--want=` on the CLI *are* the lanes MCP's
`query` tool routes to internally — same dispatcher (`(*Server).Query`), same
`QueryInput` shape, different parse-in/format-out per transport. The CLI also
carries lifecycle/ops commands with **no MCP form** — `index`, `reindex`,
`watch`, `serve`, `mcp`, `proxy`, `setup`, `doctor`, `config`, `env`, `nuke`,
`clone`, `bench`, `compact`, `compress`, `hook`, `graph <sub>`. Those build,
serve, and maintain the index (or inspect it directly) rather than query it
through the six-use-case ladder; see the README or `dex help all` — the
latter is the live, generated-from-the-binary reference for exact flags and
is the one to trust over any hand-copied table here.

## The MCP verb

| Verb | Purpose | Backend |
|----------|---------|---------|
| `query` | The one read verb. Its input **shape** picks the lane and the answer's precision tracks it: a path → compressed signatures, a `path:line`/range → that slice, a `/regex/` → grep, a bare symbol → just its call graph, a prose question → a fused semantic + symbol + graph evidence pack with a `next_action` (and, with a chat model, a cited answer). `kind=` forces the lane, `want=` the facet: `want=assemble` returns a budget-bounded working set, `kind=review` reviews the working tree. Raw file bytes are the native Read tool's job. | always (semantic lane needs an embedder; degrades to BM25 + symbol + graph) |

## Profiles, not tiers

The verb set is constant; a deployment **profile** only changes what `query` can
do internally (synthesis → lexical → hits-only), never which tools an agent sees:

- **full** (embedder + chat): `query` synthesizes cited answers.
- **bm25-only** (`DEX_EMBED_ENGINE=none`): no embedder — `query` falls back to
  BM25 + symbol + graph on its own; the semantic lane is skipped, not a missing
  tool.
- **lean** (weak local model): same single verb; `query` still routes every lane,
  only its synthesis degrades.

`DEX_EXPERT=1` is an **additive overlay**, orthogonal to the profile. Over MCP
it exposes the raw primitives `query` wraps — `search` (raw ranked hits with
the full scoring breakdown), `trace` (`--dir callers|callees|path|impact`),
`locate`, `grep`, `read` — plus `review_diff` (ref/branch/pr/worktree
targeting beyond `kind=review`'s working-tree-only scope), the graph/quality
reports `clusters`, `routes`, `smells`, `status`, `repo_map`, and the
different-contract pair `plan_rename`/`rehearse_patch` (edit plans /
hypothetical type-check, not a read).

`deps`, `cohort`, `refs`, `clones`, `similar`, and `check` are **not** separate
MCP tools (#852) — each is fully reachable as a `query(kind=...)` value with
the exact same handler and no lost capability, so a standalone door would
just be a duplicate. `clones` finds clusters of semantically near-duplicate
code blocks and `similar` finds blocks near a given one — semantic work grep
can't do; both reuse the search vectors, so they need an embedder (#84), same
as `kind=clones|similar`.

The overlay never changes the shape of `query`, the one everyday verb — on
the CLI, every `kind=` value works regardless of `DEX_EXPERT` (the flag only
gates the MCP tool surface); `dex graph <sub>` is likewise always available.
A handful of `dex graph <sub>` names have no MCP tool at all (`neighbors`,
`packages`, `links`, `backlinks`, `tags`, `cycles`, `diff`, `export`); others
(`similar`, `clones`, `routes`, `smells`, `clusters`) are the CLI door onto
the same-named DEX_EXPERT tool. `dex graph deps` is gone — that's `dex query
--kind=deps` now (#849).

## `read`

The read facet's mode contract (`ReadMode`, `internal/mcp/readmode.go`):
`full` (raw bytes, no LLM — the native Read tool's job outside dex), `signatures`
(indexed symbols + source lines), `skeleton` (exported decls in full + signatures
with `@B<n>` body handles), `map` (imports + exported symbols), `lines:N-M`
(raw line slice; also `lines:N`, `lines:N-`, `lines:-M`), `analyze` (token-cost
comparison across modes + a recommendation, no file content), and `summary`
(LLM digest — the only mode needing a chat model; without one it degrades
rather than failing hard).

The MCP `read` tool (DEX_EXPERT) carries the full surface above plus
session-scoped extras with no CLI form: `expand` (`@B<n>` body handle
expansion), `handle` (budget-downgrade re-expansion, #620), `etag`/re-read
delta diffing, `ref` (historical read as-of a git revision), and `slice`
(head/tail/range/search/json_path extraction). The CLI's `dex query
--kind=read --want=<facet>` reaches a narrower subset of the same modes; run
`dex help all` for exactly which — that surface has shifted across #849/#528
and is easiest to get stale by hand-copying here, so it isn't duplicated in
this doc.

## Response contract

Every tool returns a structured `status` (`ok`, `not-found`, `no-graph`,
`needs-chat`, `error`, …) plus a `hint`. A missing index or an offline backend
yields an explicit fallback signal, never a hard failure — so an agent can
recover (e.g. run `dex index`, retry with another mode) instead of giving up.
`query` always returns a `next_action` directive telling you what to do next.

dex is **read-only by design** (#551): every tool is `readOnlyHint: true` and
there is no edit/write/apply verb. `query` locates and explains; the host agent
makes the changes with its own editing tools. dex never writes project files, and
since #205 it holds no durable memory of its own either — findings live in the
harness's file-based memory.

## Transports

- **stdio** — `dex mcp`, attached via `claude mcp add --scope user dex -- dex mcp`.
- **HTTP** — `dex serve` exposes a versioned `/v1` REST API (bearer auth) plus
  native MCP-over-HTTP at `/v1/projects/{id}/mcp`. REST went through its own
  collapse (#851): `POST /v1/projects/{id}/query` is the one first-class,
  capability-carrying route (folding `ask`/`map`/`locate`/`cohort`/`refs`/
  `deps`/`clones` — each was a lossless duplicate front door onto a `kind=`
  value). The rest of the MCP-tool surface keeps its own dedicated route
  (`/find`, `/grep`, `/read`, `/ls`, `/trace`, `/review`, `/refactor`,
  `/callers`, `/callees`, `/impact`, `/path`, `/diff`, `/routes`, `/smells`,
  `/clusters`, `/graph/packages`) — REST was never `DEX_EXPERT`-gated the way
  stdio is, so these were never a duplicate of `/query` the way the folded
  ones were. See `internal/mcp/http.go`'s route list (each entry's comment
  says why it is or isn't folded) and [specs/http-api.md](../specs/http-api.md)
  for the exact, current set — safer to trust than a hand-copied route list
  here.
