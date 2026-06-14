# Tools

The CLI verbs and the MCP tool names are identical, so an agent and a human
share one vocabulary. CLI form: `dex <verb> [path] <args…>` (`path` defaults to
cwd). Over MCP they are tools the agent calls directly.

## Surface

| Tool     | Purpose | Backend |
|----------|---------|---------|
| `ask`    | One-shot router: picks intent, fuses lanes, returns `suggested_reads`, a `next_action`, and (with chat) a cited answer | always |
| `find`   | Hybrid semantic + lexical top-k search | embedder |
| `lookup` | Exact identifier lookup | always |
| `map`    | Deterministic repo orientation map | always |
| `trace`  | Call graph: `--dir callers\|callees\|path` | graph |
| `impact` | Transitive caller blast-radius | graph |
| `read`   | Read a file (see modes below) | always (`summary` needs chat) |
| `grep`   | Exact regex over indexed files | always |
| `ls`     | File-tree listing | always |
| `shell`  | Run a command, return compressed output | always |
| `deps` `callers` `callees` `path` `diff` `clusters` `routes` `smells` `notes` `session` | Graph/analysis power lane | graph |

## Capability-derived exposure

A tool is registered only when the backend it needs is available, so the surface
matches the deployment:

- **Always on** (no models at all): `ask`, `grep`, `ls`, `shell`.
- **Default verbs** (non-weak model): add `map`, `trace`, `impact`, `read`.
- **`find`**: only when a query-time embedder is wired; otherwise retrieval
  degrades to BM25 + symbol + graph and `ask` routes around it.
- **Power lane** (`lookup`, `deps`, `callers`, `callees`, `path`, `diff`,
  `clusters`, `routes`, `smells`, `notes`, `session`): behind `DEX_EXPERT=1`, to
  keep the everyday agent tool list small. (On the CLI every verb is always
  available.)
- **Weak/local model detected**: only the always-on lane is exposed.

This is a flat, prefix-free surface of up to 21 tools — no `category_` prefixes,
no tiers.

## `read` modes

`read --mode=<m>` (default `full`):

| mode | output | LLM |
|------|--------|-----|
| `full` | raw file content (the default) | no |
| `signatures` | indexed symbols + their source lines | no |
| `skeleton` | exported decls in full + function signatures with `@B<n>` body handles | no |
| `map` | imports + exported symbols | no |
| `aggressive` | maximal lossy compression: strips comments and low-entropy lines (declaration + control-flow lines protected) | no |
| `lines:N-M` | a raw line slice | no |
| `summary` | LLM-generated digest (`--focus` to steer) | **yes** |

`summary` is the only mode that needs a chat model; without one it returns
`status=needs-chat` and you fall back to a structural mode.

The CLI `read` verb adds two local conveniences with no MCP equivalent —
`entropy` (entropy-ranked compression) and `auto` (large indexed files →
`signatures`, else `full`, mirroring the redirect hook). Conversely the MCP
tool's session-scoped `expand` (`@B<n>` body handles) and internal `handle`
downgrade have no CLI form: handles live in per-session server memory. CLI↔MCP
mode parity is locked by `cmd/dex/read_parity_test.go`.

## Response contract

Every tool returns a structured `status` (`ok`, `not-found`, `no-graph`,
`needs-chat`, `error`, …) plus a `hint`. A missing index or an offline backend
yields an explicit fallback signal, never a hard failure — so an agent can
recover (e.g. run `dex index`, retry with another mode) instead of giving up.
`ask` always returns a `next_action` directive telling you what to do next.

## Transports

- **stdio** — `dex mcp`, attached via `claude mcp add --scope user dex -- dex mcp`.
- **HTTP** — `dex serve` exposes the same tools as a versioned `/v1` REST API
  (bearer auth) plus native MCP-over-HTTP at `/v1/projects/{id}/mcp`. The REST
  routes share the tool names and are always full (`DEX_EXPERT` only trims the
  stdio surface). See [specs/http-api.md](../specs/http-api.md).
