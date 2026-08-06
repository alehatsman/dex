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

Signature: `resolveProject(ctx, req, projectRoot) (*proj.Project, string /*source*/, string /*errHint*/)`.
`source ∈ {"arg","client-root","cwd"}`. The third return keeps today's
error-hint semantics (handlers already do `if hint != "" { …Status:"error"… }`),
so error handling at the 35 sites is unchanged; they additionally gain `source`.

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

dex already half-does this: `SearchOutput.Project` / `LocateOutput.Project` are
commented *"resolved project root."* Standardize and strengthen it:

- Every tool that resolves a project echoes the resolved **root path** in its
  existing `Project` field (fill the gaps where it's missing).
- Add the **source** so a fallback pick reads as one, e.g.
  `project: "/…/dex (resolved from client workspace root — pass project_root to
  target a different worktree)"` when `source != "arg"`. Empty/plain when the
  caller passed an explicit arg (no noise on the happy path).
- When `source == "cwd"`, also emit a **deduped stderr warning** (one line per
  distinct fallback root per process, via `s.AutoWatch.Logger`, nil-guarded):

  ```
  mcp: resolved project_root from server cwd (<wd>); pass project_root or start
  Claude inside the worktree to target it
  ```

Why this and not a per-result banner injected at the `addTool` adapter: the SDK
synthesizes result text from the typed `Out` when `Content` is nil, so injecting
a banner there suppresses that rendering for some tools. Reusing the existing
`Project`/`Hint` field convention is uniform with dex's own idiom and low-risk.
A universal provenance envelope across *all* 35 tools is deferred to the #110
envelope work — pillar C covers the high-traffic tools that already carry the
field, which is where the footgun bites.

## Call-site update (35 sites)

Mechanical: `s.resolveProject(in.ProjectRoot)` →
`s.resolveProject(ctx, req, in.ProjectRoot)`, LHS `p, src, hint := …` (rename the
discarded `_ *sdk.CallToolRequest` param to `req` where needed). Error handling
line unchanged. High-traffic outputs additionally set `Project` from `p.Root` +
`src`. CLI wrappers (`s.check(ctx, nil, in)`) pass `nil` → roots skipped, cwd
backstop + warn. `server_summarize.go:432`'s separate `resolveProjectRoot` free
function is brought under the same precedence (same helper).

Behavior-neutral for the two dominant callers: explicit-`project_root` MCP calls
and CLI (`nil` session).

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
  `file:///<tmp-worktree>`; `resolveProject(ctx, req, "")` yields `p.Root ==
  worktree`, `source == "client-root"`, **not** cwd. (The worktree coverage #120
  requires.)
- `TestRootFromClient_{Empty,ListError,NonFileURI,NonexistentDir}` → each "".
- `TestResolveProjectPrecedence` — table over {arg, roots, cwd}: arg > roots >
  cwd; source label correct at each rung; cwd emits the warn.
- `TestResolveProject_ExplicitArgSkipsRoots` — fake lister asserts `ListRoots`
  is **not** called when the arg is non-empty (guards the round-trip cost).
- `TestProjectRootDesc_SingleSource` — the centralized const is what the tools
  advertise (guards against a reintroduced "defaults to cwd" copy).

Fakes: `stubLister` implementing `rootLister`; `t.TempDir()` for roots/cwd.

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
