# Tools

This is the **query/tool contract**. Since the #205 single-verb cutover the agent
surface (over MCP) and the human surface (on the CLI) **diverge on purpose**:

- **Over MCP** an agent sees **one verb** — `query` (read). That set is constant
  across every deployment profile. dex is retrieval over the codebase, not agent
  memory: the `record` write verb and the L3 knowledge subsystem were removed in
  #205 — durable findings are the harness's file-based memory now.
- **On the CLI** the granular verbs live on directly (`dex ask` / `search` /
  `read` / `locate` / `trace` / `review_diff` / `check` / `grep`), because a
  human at a prompt wants to name the lane. CLI form:
  `dex <verb> [path] <args…>` (`path` defaults to cwd).

The two are one engine: the CLI verbs *are* the lanes `query` routes to
internally. The CLI also carries lifecycle/ops commands with **no MCP form** —
`index`, `reindex`, `watch`, `serve`, `mcp`, `proxy`, `setup`, `doctor`,
`config`, `env`, `nuke`, `clone`, `bench`, `compact`, `compress`, `hook`. Those
build, serve, and maintain the index rather than query it; see the README or
`dex help all`.

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

`DEX_EXPERT=1` is an **additive overlay**, orthogonal to the profile. It exposes
the raw primitives `query` wraps — `search` (raw ranked hits with the full
scoring breakdown), `trace` (`--dir callers|callees|path|impact`), `locate`,
`grep`, `read` — plus the graph/quality reports `clusters`, `routes`,
`smells`, `status`, `repo_map`, `review_diff`, `plan_rename`, and
`rehearse_patch`.

`deps`, `cohort`, `refs`, `clones`, `similar`, and `check` are **not** separate
tools (#852, query-unification MCP re-justification) — each is fully reachable
as a `query(kind=...)` value with the exact same handler and no lost
capability, so a standalone door would just be a duplicate. `clones` finds
clusters of semantically near-duplicate code blocks and `similar` finds blocks
near a given one — semantic work grep can't do; both reuse the search vectors,
so they need an embedder (#84), same as `kind=clones|similar`.

The overlay never changes the shape of `query`, the one everyday verb.

On the CLI every verb, plus the full `dex graph <sub>` set, is always available
without the flag. Several `dex graph` subcommands are **CLI-only** with no MCP
tool — `neighbors`, `packages`, `links`, `backlinks`, `tags`, `cycles`,
`export`.

## `read` modes

`read --mode=<m>` (default `full`):

| mode | output | LLM |
|------|--------|-----|
| `full` | raw file content (the default) | no |
| `signatures` | indexed symbols + their source lines | no |
| `skeleton` | exported decls in full + function signatures with `@B<n>` body handles | no |
| `map` | imports + exported symbols | no |
| `aggressive` | maximal lossy compression: strips comments and low-entropy lines (declaration + control-flow lines protected) | no |
| `lines:N-M` | a raw line slice; also `lines:N` (single line), `lines:N-` (line N → end of file), `lines:-M` (first M lines) | no |
| `analyze` | token-cost comparison of every mode + a recommended mode + a `handle`; **no file content** — pick the cheapest view first, or analyze many files then expand only the ones you need via `read(handle=…, mode=…)` (#620) | no |
| `summary` | LLM-generated digest (`--focus` to steer) | **yes** |

`summary` is the only mode that needs a chat model; without one it returns
`status=needs-chat` and you fall back to a structural mode.

The CLI `read` verb adds two local conveniences with no MCP equivalent —
`entropy` (entropy-ranked compression) and `auto` (large indexed files →
`signatures`, else `full`, mirroring the redirect hook). Conversely the MCP
tool's session-scoped `expand` (`@B<n>` body handles) and internal `handle`
downgrade have no CLI form: handles live in per-session server memory. CLI↔MCP
mode parity is locked by `cmd/dex/read_parity_test.go`.

Line ranges differ in spelling between the two: the CLI uses the `--start` /
`--end` flags (1-based, `0` = open), while the MCP tool uses the `lines:*` mode
string. So `read --start=10 --end=40` ≡ `mode=lines:10-40`, `--start=10 --end=0`
≡ `lines:10-`, and `--start=0 --end=20` ≡ `lines:-20`.

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
- **HTTP** — `dex serve` exposes the same tools as a versioned `/v1` REST API
  (bearer auth) plus native MCP-over-HTTP at `/v1/projects/{id}/mcp`. The REST
  routes share the tool names and are always full (`DEX_EXPERT` only trims the
  stdio surface). See [specs/http-api.md](../specs/http-api.md).
