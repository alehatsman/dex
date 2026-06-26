# Tools

This is the **query/tool contract** — the verbs an agent (over MCP) and a human
(on the CLI) share. Their names are identical, so the two surfaces speak one
vocabulary. CLI form: `dex <verb> [path] <args…>` (`path` defaults to cwd); over
MCP they are tools the agent calls directly.

The CLI *also* carries lifecycle/ops commands that have **no MCP form** and are
not part of this contract — `index`, `reindex`, `watch`, `serve`, `mcp`,
`proxy`, `setup`, `doctor`, `config`, `env`, `clone`, `bench`, `compact`,
`compress`, `hook`. Those build, serve, and maintain the index rather than query
it; see the README or `dex help all`.

## Surface

| Tool     | Purpose | Backend |
|----------|---------|---------|
| `ask`    | One-shot router: picks intent, fuses lanes, returns `suggested_reads`, a `next_action`, and (with chat) a cited answer | always |
| `find`   | Hybrid semantic + lexical top-k search | embedder |
| `map`    | Deterministic repo orientation map | always |
| `trace`  | Call graph: `--dir callers\|callees\|path\|impact` (impact = transitive caller blast-radius + risk tier + `tests_to_run`) | graph |
| `locate` | One-call orientation around `ref` (`path:line`) / `symbol` / `frame`: callers, sibling tests, nearest doc, last commit, related notes | always (callers need graph) |
| `review` | Per-hunk PR intelligence for a `ref` / `branch` / `pr`: touched symbols, callers (+ risk tier), tests, nearest doc, churn, author history, notes (+ per-file scope-bound notes, #645) | always (callers need graph) |
| `refactor` | Plan a type-precise rename → byte-exact edit triples to apply yourself (never writes). Go-only v1 | Go toolchain |
| `verify` | Run the tests a change implicates — working tree (default) / `ref` range / `symbol` blast-radius — and return pass/fail; routes through the shell pipeline so a failing run stages a `gotcha_candidate`. Override the command via `command` / `$DEX_VERIFY_CMD` (`{{packages}}`). Go-only v1 | Go toolchain |
| `check`  | Verify a batch of `file:line[:symbol]` references against the index — returns `ok \| moved \| gone \| no_file \| parse_error` per claim; `moved` includes `found_at`. Use after code changes to confirm cited locations are still valid. | always |
| `read`   | Read a file (see modes below) | always (`summary` needs chat) |
| `grep`   | Exact regex over indexed files | always |
| `shell`  | Run a command, return compressed output | always |
| `notes`  | Persistent project memory: `add`/`list`/`delete`/`gc` facts; high-salience ones auto-inject into `ask`; `add` warns (`similar`) on a near-duplicate note | always |
| `deps` `diff` `clusters` `routes` `smells` `cohort` `status` `budget` `session` `checkpoint` | DEX_EXPERT power lane (`checkpoint`: shadow-git work history) | graph (`cohort`: Go toolchain) |

## Capability-derived exposure

A tool is registered only when the backend it needs is available, so the surface
matches the deployment:

- **Always on** (no models at all): `ask`, `grep`, `shell`.
- **Default verbs** (non-weak model): add `map`, `trace` (incl. `--dir impact`), `locate`, `review`, `refactor`, `read`, `notes`.
- **`find`**: only when a query-time embedder is wired; otherwise retrieval
  degrades to BM25 + symbol + graph and `ask` routes around it.
- **Power lane** (`deps`, `diff`, `clusters`, `routes`, `smells`, `cohort`,
  `status`, `budget`, `session`, `checkpoint`): behind `DEX_EXPERT=1`, to keep the everyday agent tool list small.
  Call-graph walks (callers/callees/shortest path) are not standalone tools —
  `trace --dir callers|callees|path` is the single entry point. (On the CLI
  every verb, plus the full `dex graph <sub>` set, is always available.)
  Several `dex graph` subcommands are **CLI-only** with no MCP tool —
  `neighbors`, `packages`, `links`, `backlinks`, `tags`, `cycles`, `export` —
  so they don't count toward the tool surface above.
- **Weak/local model detected**: only the always-on lane is exposed.

This is a flat, prefix-free surface of up to 18 tools — no `category_` prefixes,
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
there is no edit/write/apply verb. dex locates and explains (`find`, `trace`,
`read`); the host agent makes the changes with its own editing tools.
The only persistence verb, `notes`, writes dex's knowledge store — never project
files.

## Transports

- **stdio** — `dex mcp`, attached via `claude mcp add --scope user dex -- dex mcp`.
- **HTTP** — `dex serve` exposes the same tools as a versioned `/v1` REST API
  (bearer auth) plus native MCP-over-HTTP at `/v1/projects/{id}/mcp`. The REST
  routes share the tool names and are always full (`DEX_EXPERT` only trims the
  stdio surface). See [specs/http-api.md](../specs/http-api.md).
