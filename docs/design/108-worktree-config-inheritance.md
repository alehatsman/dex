# 108 — Worktree config inheritance

## Problem

Working in a **git worktree**, dex is blind: the worktree is never indexed.
`index.include` is opt-in and lives in `.dex/config.yml`; a fresh linked
worktree has no such file, so `ignore.New` reports `IncludeConfigured() ==
false` and **every file is skipped** — `dex index <worktree>` prints "nothing
will be indexed", `dex status <worktree>` says "not indexed", and the MCP
watcher's `CheckIndexable`/index pass produces an empty index.

Net: for a one-worktree-per-task workflow, dex is wrong-by-default every
session until the parent's `.dex/config.yml` is copied in by hand.

## Scope

**In scope (root cause):** a linked worktree with no local `.dex/config.yml`
inherits config from its **main working tree**, so `index.include` (and the
DEX_* knobs) resolve exactly as they do in the main checkout. This is the
`nothing-will-be-indexed` wrong-by-default.

**Out of scope (separate follow-up):** MCP default `project_root` resolution.
When no `project_root` is passed, `Server.resolveProject` falls back to the
server's launch cwd (the main repo). Over MCP **stdio the server cannot see the
client's cwd**, so it cannot resolve to the caller's worktree; Claude Code
already passes `project_root` explicitly. Echoing the resolved root in every
tool result is likewise deferred. File as #108-follow-up.

## Design

A linked worktree's `.git` is a **file** (`gitdir: <main>/.git/worktrees/<name>`),
not a directory. The main working tree is derivable purely from the filesystem —
no `git` exec, keeping `internal/ignore` a leaf with no new process dependency:

1. Read `<root>/.git`. If it is a directory → not a linked worktree; stop.
2. Parse the `gitdir: <path>` line → the worktree's git dir.
3. Require the git dir to contain a `worktrees/` path segment (this is what
   distinguishes a linked worktree from a submodule, whose gitdir is
   `…/.git/modules/<name>`). If absent → stop.
4. Read `<gitdir>/commondir` (typically `../..`), resolve it relative to the
   git dir → the **common** git dir (`<main>/.git`).
5. Main working tree = `filepath.Dir(commonGitDir)`.

Validated against a real worktree:
`.git` → `gitdir: /Users/.../dex/.git/worktrees/fix-108`, `commondir` = `../..`
→ `/Users/.../dex/.git`, parent = `/Users/.../dex` (has `.dex/config.yml`). ✓

### New helper — `internal/gitworktree`

A tiny leaf package (no deps beyond stdlib), one function:

```go
// MainWorktree reports the main working tree of a linked git worktree.
// ok is false when root is not a linked worktree (its .git is a directory,
// or a submodule gitdir), so callers fall through to their normal behavior.
func MainWorktree(root string) (mainRoot string, ok bool)
```

Pure filesystem, best-effort: any read/parse failure returns `("", false)` —
inheritance is a convenience, never a hard dependency.

### Wiring — config loaders inherit on miss

Both readers already key off `<root>/.dex/config.yml` and treat a missing file
as a no-op. Add the same fallback to each: **if the local file is absent and
`MainWorktree(root)` is ok, read the main working tree's file instead.** The
local file always wins when present (a worktree may override).

- `internal/ignore/config.go` `loadIndexConfig` — the critical half (fixes
  `index.include` → makes indexing produce files).
- `cmd/dex/config_file.go` `parseConfigFile` — DEX_* env knobs, for parity so
  endpoints/models resolve identically in a worktree.

No change to `Matcher`, `CheckIndexable`, or the MCP watcher: once
`loadIndexConfig` returns the inherited include, every downstream gate
(`IncludeConfigured`, `warnIfNoInclude`, doctor's `project cfg`) goes green
unchanged.

## Edge cases

- **Main working tree** (`.git` is a dir): `ok=false`, no fallback — unchanged.
- **Submodule** (`gitdir …/.git/modules/<name>`, no `worktrees/` segment):
  `ok=false` — deliberately not inherited.
- **Worktree WITH its own `.dex/config.yml`**: local file wins; fallback never
  consulted (reader returns before the fallback branch).
- **Worktree outside the repo** (`~/worktrees/<repo>/<branch>`): gitdir still
  points into the main repo's `.git/worktrees/`, so resolution is identical.
- **Main working tree missing `.dex/config.yml`**: fallback read misses too →
  same zero-value config as today (still index-nothing, warned) — no regression.
- **Relative `commondir`**: resolved against the git dir with `filepath.Join`
  then cleaned; absolute `commondir` used as-is.

## Validation

- New `internal/gitworktree` unit test: real `git worktree add` under a temp
  repo → `MainWorktree(wt)` returns the main root, `ok=true`; main root and a
  plain non-git dir → `ok=false`; a synthesized submodule `.git` file → `ok=false`.
- `internal/ignore` test: worktree dir with no local config but a main worktree
  carrying `index.include` → `IncludeConfigured() == true` and Match honors the
  inherited allow-list; local config still overrides.
- `cmd/dex` config test: `parseConfigFile(worktree)` inherits DEX_* from the
  main worktree; local file still wins.
- `mooncake task ci` green; `dex index`/`status`/`doctor` output on the main
  checkout unchanged (fallback only fires for a linked worktree with no local
  config).

## Interfaces touched

| File | Change |
|------|--------|
| `internal/gitworktree/gitworktree.go` (new) | `MainWorktree` helper + doc |
| `internal/gitworktree/gitworktree_test.go` (new) | unit tests |
| `internal/ignore/config.go` | `loadIndexConfig` inherits on local miss |
| `cmd/dex/config_file.go` | `parseConfigFile` inherits on local miss |
| `cmd/dex/doctor.go` | `checkProjectConfig` reports inherited config instead of the now-false "nothing will be indexed" |
| tests in `internal/ignore`, `cmd/dex` | inheritance coverage |

## Note — doctor consistency

`doctor`'s `project cfg` check does its own direct `os.Stat` of the config file
(separate from the indexer's `ignore.New` gate). Once the indexer inherits, an
un-updated doctor would print "no .dex/config.yml → nothing indexed" while
indexing actually works — a contradictory diagnostic. `checkProjectConfig` is
made inherit-aware to match: a linked worktree inheriting a parent config reports
`✓ inherited from <main> (worktree)`.
