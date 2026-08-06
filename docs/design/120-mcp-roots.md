# 120 — resolve the project root from MCP roots, tell the agent when we can't

Status: proposed
Issue: #120 (Refs #108)

## Goal

A root-less MCP tool call must resolve to the **caller's actual workspace**, not
the dex server's launch cwd. Where dex genuinely cannot know the workspace
(the common "start Claude in the main repo, then tell it to work in a worktree"
flow), the agent must be *told* to pass the worktree path, and any fallback must
be **visible in the result** — never a silent main-repo read.

## The scenario that drives this

The dominant real-world flow (the maintainer's daily loop; will be common as dex
gets more users):

1. `claude` is started in the **main** checkout (`/…/dex`). Claude Code's
   workspace root = that dir. It spawns `dex mcp` (stdio); dex's cwd = main.
2. Mid-session the user says "work in the worktree `~/worktrees/dex/feat-x`."
   Claude edits there with native `Edit`/`Write` (path-based — fine).
3. Claude calls a dex tool to understand the code. If it **omits** `project_root`,
   dex resolves to **main** and searches the wrong index — silently.

Two hard truths this spec is built around:

- **Roots alone does not fix this flow.** MCP `roots` reflects the *session's*
  workspace (launch dir + `/add-dir`), which here is **main**, not the worktree.
  Roots fixes the *other* flow — one `claude` started **inside** each worktree,
  where the session root *is* the worktree.
- **dex cannot infer the agent's edit location** from a bare tool call. Only an
  explicit `project_root` targets the worktree. So the fix is three-part: resolve
  from roots when we can, *tell the agent* to pass the path when we can't, and
  make the resolved root **legible** so a wrong pick is caught, not silent.

## Root cause (cited)

`internal/mcp/server.go:556` — the single chokepoint, 35 callers:

```go
func (s *Server) resolveProject(projectRoot string) (*proj.Project, string) {
	root := projectRoot
	if root == "" {
		wd, _ := os.Getwd()   // ← server launch cwd, NOT the caller's worktree
		root = wd
	}
	...
}
```

And the description every handler advertises to the agent
(`server_search.go:35` and ~a dozen copies) actively *encourages* omission:

```
project_root: absolute path to the project root; defaults to the server's working directory
```

That sentence tells the model "safe to leave blank," which is precisely wrong in
the worktree flow. It is a root cause, not just a resolver bug.

## What the ecosystem does (research)

MCP's **`roots` capability** is the protocol-native answer: the client declares
its workspace dirs at init; the server calls `roots/list`. The reference
filesystem server resolves from roots first; Claude Code's guidance tells
file-touching servers to "implement `roots/list` and use Claude Code's
working-directory model instead of relying on cwd." Confirmed available in the
SDK dex ships (`modelcontextprotocol/go-sdk v1.6.1`):

- `ServerSession.ListRoots(ctx, *ListRootsParams)` (`mcp/server.go:1174`).
- Every handler gets `*sdk.CallToolRequest`; `ServerRequest.Session` carries the
  live session (`mcp/shared.go:478`). `Root{URI file://…, Name}`
  (`mcp/protocol.go:1187`).
- stdio is 1:1 client↔process, so each session's roots are its own — correct per
  connection *when the session is rooted at the worktree*.

## Design — three pillars

### A. Resolver: `arg → roots → cwd`

Make the chokepoint session-aware. When `project_root` is empty:

1. **explicit arg** — used verbatim (unchanged; all existing callers).
2. **client roots** — if a live session is present, `ListRoots`; use the first
   `file://` root that `proj.Resolve` accepts. Fixes the start-in-worktree flow.
3. **`os.Getwd()`** — last resort, **plus** a deduped stderr warning.

Deliberately **no `DEX_PROJECT_ROOT` env var**: redundant. Roots covers rooted
clients; the explicit arg covers deliberate callers; cwd already lets an operator
pin a single-project server by launching it in the repo. A shared multi-project
server pinned to one root would be *wrong* anyway. New env surface for a contrived
gap the stderr warning already signals — skip it (explicit > magic, minimal).

Testability seam — `ServerSession` is concrete, so resolution goes through a tiny
interface it already satisfies; tests inject a fake:

```go
type rootLister interface {
	ListRoots(context.Context, *sdk.ListRootsParams) (*sdk.ListRootsResult, error)
}

// rootFromClient asks the client for its workspace roots and returns the first
// file:// root that resolves to an existing dir, or "" on any error / empty /
// unsupported capability / timeout. Never fatal — caller falls through to cwd.
func rootFromClient(ctx context.Context, l rootLister, base string) string
```

- 2s timeout so a slow client can't hang a tool call.
- `net/url`-parse `file://` → path; skip non-`file://` / unparseable.
- First root that `proj.Resolve(path, base)` accepts wins (deterministic); none →
  "" → cwd.

Signature: `resolveProject(ctx, projectRoot) (*proj.Project, string /*errHint*/)`
— arity unchanged (avoids a source-label return the callers don't need), the
error-hint semantics are untouched, and the **session reaches the resolver via
`ctx`, not a new `req` param**: `addTool` (the single handler adapter) stashes
`req.Session` into the context (`withSession`), and `resolveProject` recovers it
(`listerFromContext`). So the 35 call sites change by inserting `ctx` only — no
`_ *sdk.CallToolRequest`→`req` renames (which would trip revive's
`unused-parameter` on handlers that don't otherwise touch the request). The CLI
path calls handlers with a nil request, so no session is stashed and resolution
falls straight through to cwd.

### B. Tell the agent to target the worktree (the fix for the driving scenario)

Rewrite the `project_root` description from a "safe to omit" hint into an
imperative that names the worktree case. Centralize the ~dozen copies into one
`const projectRootDesc` so it can't drift:

```
project_root: absolute path to the project or git worktree you are working in.
The server cannot see your shell's directory — when your edits are in a worktree
that differs from where the MCP server was started, pass that worktree's path.
Omit only when working in the server's own workspace.
```

Plus one line in the generated Claude routing-rule block
(`setup.go` `rulesContent`, bumped to `dex-rules-v4`):

> Working in a git worktree? Pass its absolute path as `project_root` to every
> dex call — the server resolves the main checkout by default.

This is the cheap, high-leverage part: a well-behaved Claude that reads the param
will pass the worktree path, and the driving scenario just works.

### C. Make the resolved root legible (safety net when B is forgotten)

The resolved root is **already** echoed in every tool result: nearly all outputs
carry `Project string json:"project,omitempty"` set to `p.Root`
(`SearchOutput.Project` et al are commented *"resolved project root"*). So the
resolution is not silent today — the result always states which project it hit;
it is just easy to miss. Pillar C adds the one missing signal without disturbing
that convention:

- **Deduped stderr warning on the cwd backstop** — `warnCwdFallback`, one line
  per distinct fallback root per process (package `sync.Map` guard), via the
  package `slog` (matching the existing panic-guard logging in `addTool`):

  ```
  resolved project_root from server cwd; pass project_root or start the client
  inside the worktree to target it  cwd=<wd>
  ```

  This fires only when there was no explicit arg *and* no usable client root —
  exactly the case pillar B is meant to prevent, now made loud.

Deliberately **not** injecting a per-result provenance banner at the `addTool`
adapter, nor threading a `source` label into the 35 outputs: the existing
`Project` echo already carries the resolved path uniformly, and the SDK
synthesizes result text from the typed `Out` when `Content` is nil, so an
adapter-level banner would suppress that rendering for some tools. A *uniform*
"resolved from X, pass project_root to retarget" provenance line belongs in the
#110 universal envelope, where every tool result gets consistent provenance —
tracked as a follow-up rather than bolted onto individual hot handlers here (their
hint-priority ladders make ad-hoc injection fragile).

## Call-site update (35 sites)

Mechanical: `s.resolveProject(in.ProjectRoot)` → `s.resolveProject(ctx,
in.ProjectRoot)`. LHS and error handling unchanged. Three handlers named their
context param `_ context.Context` (budget, index_status, summarizeResolveMode) —
those get the param named `ctx` (now used, so no `unused-parameter` violation).
`addTool` gains one line (`withSession(ctx, req.Session)`). The separate
`resolveProjectRoot` free function (`server_summarize.go`) becomes a method
`(s *Server) resolveProjectRoot(ctx, projectRoot)` under the same precedence.

Behavior-neutral for the two dominant callers: explicit-`project_root` MCP calls
and CLI (nil request → no session → cwd path).

## Edge cases

| Case | Behavior |
|---|---|
| Explicit `project_root` | Used verbatim; roots never consulted; no provenance noise. |
| Start-in-worktree (session root = worktree) | Roots returns worktree → correct, zero agent effort. |
| Start-in-main, work-in-worktree, arg omitted | Roots returns **main** → resolves main, `source="client-root"`, `Project` echoes main + hint. Agent/user sees the mismatch and corrects. (Pillar B is what prevents it up front.) |
| Client doesn't support roots | `ListRoots` errors → "" → cwd + warn. No regression. |
| Client returns empty roots | "" → cwd + warn. |
| Root URI not `file://` / nonexistent dir | Skipped; try next; none → cwd. |
| Multiple roots | First that resolves wins (deterministic); echoed via `Project`. |
| CLI path (`req == nil`) | Roots skipped; cwd + warn. Unchanged behavior. |
| `ListRoots` hangs | 2s timeout → "" → cwd + warn. Never blocks. |
| HTTP transport, many sessions | `req.Session` is the per-request session; resolver reads it from the request, not from `s` → no shared-state race. |

## Test plan

New `internal/mcp/roots_test.go`:

- `TestRootFromClient_PicksWorktree` — fake `rootLister` returns
  `file:///<tmp-worktree>`; `rootFromClient` returns that path, and
  `resolveProject(withSession(ctx, fake), "")` yields `p.Root == worktree`,
  **not** cwd. (The worktree coverage #120 requires.)
- `TestRootFromClient_{Empty,ListError,NonFileURI,NonexistentDir}` → each "".
- `TestResolveProjectPrecedence` — arg wins over a fake root; a fake root wins
  over cwd; no session ⇒ cwd.
- `TestResolveProject_ExplicitArgSkipsRoots` — fake lister records that
  `ListRoots` is **not** called when the arg is non-empty (guards the round-trip
  cost on the common Claude-Code path).
- `TestFileURIToPath` — `file:///a/b` → `/a/b`; bare `/a/b` → `/a/b`;
  `http://x` / `""` → `"".`

Fakes: a `stubLister` implementing `rootLister` (returns a canned
`*sdk.ListRootsResult`, flips a `called` flag); `t.TempDir()` for roots/cwd. The
`ctx`-stash seam means tests drive `resolveProject` through `withSession` without
fabricating an SDK `CallToolRequest`.

Pillar B is guarded by reflecting a representative input type's `ProjectRoot`
`jsonschema` tag (struct tags can't hold a shared const, so the guard asserts the
stale "defaults to the server's working directory" phrasing is gone and the
worktree guidance is present).

## Validation / rollout

- `mooncake task ci-fast` green.
- `mooncake task install`; empirically confirm what Claude Code returns from
  `roots/list` in a worktree session vs. a main session (log one `ListRoots`
  payload) — documents real client behavior. Graceful fallback makes the change
  safe regardless.
- Manual: start `claude` in main, drive a dex call about a worktree with the
  arg omitted → result `Project` shows main + the "pass project_root" hint;
  with the arg → clean worktree resolution.

## Follow-ups filed after merge

- `notifications/roots/list_changed` subscription + per-session roots cache
  (avoid a `ListRoots` round-trip on every root-less call).
- Universal provenance in the #110 envelope: every tool result states which root
  it used and how (retire the per-field `Project` convention).
- Optional: a `dex worktree` helper (or documented snippet) that spawns `claude`
  inside a fresh worktree so roots resolves it automatically — the ergonomic
  path that sidesteps the whole footgun. (Maintainer flagged interest in
  building this client-side.)
