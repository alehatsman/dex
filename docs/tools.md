# Tools

This is the **query/tool contract**. Since the #110 cutover the agent surface
(over MCP) and the human surface (on the CLI) **diverged on purpose**:

- **Over MCP** an agent sees **four verbs** — `ask`, `look`, `act`, `remember` —
  plus `index_status`. That set is constant across every deployment profile.
- **On the CLI** the granular verbs live on directly (`dex ask` / `search` /
  `read` / `locate` / `trace` / `review` / `verify` / `grep` / `notes`), because
  a human at a prompt wants to name the lane. CLI form:
  `dex <verb> [path] <args…>` (`path` defaults to cwd).

The two are one engine: the CLI verbs *are* the lanes `ask` and `look` route to
internally. The CLI also carries lifecycle/ops commands with **no MCP form** —
`index`, `reindex`, `watch`, `serve`, `mcp`, `proxy`, `setup`, `doctor`,
`config`, `env`, `nuke`, `clone`, `bench`, `compact`, `compress`, `hook`. Those
build, serve, and maintain the index rather than query it; see the README or
`dex help all`.

## The four MCP verbs

| Verb | Purpose | Backend |
|----------|---------|---------|
| `ask` | Front door: routes intent (behavior_search / symbol_lookup / callers / callees / architecture / package_topology / editing_context / assemble / review), fuses the semantic + symbol + graph lanes, returns a ranked evidence pack, a `next_action`, and (with a chat model) a cited answer. `ask("review my changes")` reviews the working tree. | always (semantic lane needs embedder; degrades to BM25 + symbol + graph) |
| `look` | Exact fetch for a target you can already name — dex classifies it: a path → read, a `/regex/` → grep, a `path:line` → locate, a symbol → its call graph. Every result carries `trust: exact`. | always (call graph needs graph) |
| `act` | Run a command, get compressed output inside the universal `{result, trust, cost, next}` envelope. Blocks writes (`>`/`>>`/`tee`) unless `DEX_SHELL_ALLOW_WRITES=1`; 60 s timeout; `raw:true` to skip compression. | always |
| `remember` | Durable project memory: write a fact (optionally `scope`-bound to a glob), recall the most relevant by `query`, or correct a stale one with `supersedes=<id>`. High-salience facts auto-inject into `ask` as `knowledge_facts`. | always |

## Profiles, not tiers

The verb set is constant; a deployment **profile** only changes what `ask` can do
internally (synthesis → lexical → hits-only), never which tools an agent sees:

- **full** (embedder + chat): `ask` synthesizes cited answers.
- **bm25-only** (`DEX_EMBED_ENGINE=none`): no embedder — `ask` falls back to
  BM25 + symbol + graph on its own; the semantic lane is skipped, not a missing
  tool.
- **lean** (weak local model): same four verbs; `remember` matters most here,
  since a weaker model forgets more.

`DEX_EXPERT=1` is an **additive overlay**, orthogonal to the profile. It exposes
the raw primitives the verbs wrap — `search` (raw ranked hits with the full
scoring breakdown), `trace` (`--dir callers|callees|path|impact`), `locate`,
`grep`, `read`, `shell` — plus the graph/quality lanes `deps`, `diff`,
`clusters`, `routes`, `smells`, `clones`, `similar`, `cohort`, `status`,
`session`, `checkpoint`, `refactor`, `rehearse`, `check`, and the full `notes`
knowledge surface. `clones` finds clusters of semantically near-duplicate code
blocks and `similar` finds blocks near a given one — semantic work grep can't do;
both reuse the search vectors, so they need an embedder (#84). The overlay never
changes the shape of the four everyday verbs.

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
`ask` always returns a `next_action` directive telling you what to do next.

dex is **read-only by design** (#551): every tool is `readOnlyHint: true` and
there is no edit/write/apply verb. `ask` and `look` locate and explain; the host
agent makes the changes with its own editing tools. The only persistence verb,
`remember`, writes dex's knowledge store — never project files.

## Transports

- **stdio** — `dex mcp`, attached via `claude mcp add --scope user dex -- dex mcp`.
- **HTTP** — `dex serve` exposes the same tools as a versioned `/v1` REST API
  (bearer auth) plus native MCP-over-HTTP at `/v1/projects/{id}/mcp`. The REST
  routes share the tool names and are always full (`DEX_EXPERT` only trims the
  stdio surface). See [specs/http-api.md](../specs/http-api.md).
