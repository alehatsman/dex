# Contributing to dex

## Workflow

Work is tracked in [moongit](https://moongit.dev) issues. The loop is:

1. **Survey open work** — `mgit issue list --state todo,in_progress`
2. **Create or pick an issue** — one issue per unit of work, plan in the body
3. **Claim before coding** — `mgit issue claim <n> --state in_progress`
   Don't start work already `in_progress` under another identity.
4. **Open a worktree** — `git worktree add ../dex-<n>-slug -b feat/<n>-slug`
   Every change, including small ones, gets a worktree. Never edit directly on `main`.
5. **Build and test via mooncake** — do not run bare `go build` or `go test`:
   ```sh
   mooncake task ci-fast   # pre-commit gate (build + test + vet + fmt)
   mooncake task test      # full test suite
   mooncake task ci        # full pre-push gate
   ```
   The project requires `-tags sqlite_fts5`; mooncake threads this automatically.
6. **Commit** with a [Conventional Commits](https://www.conventionalcommits.org/)
   message: `feat(pkg):`, `fix(pkg):`, `docs:`, `refactor(pkg):`, `test(pkg):`, etc.
7. **Push and open a PR** — `mgit pr create --base main --head <branch> --title "…" --body "…"`
8. **Merge fast-forward only** — `mgit pr merge <n> --ff-only`
   If `main` advanced, rebase your branch first: `git rebase origin/main`
9. **Close the issue** — `mgit issue set-state <n> done`
10. **Remove the worktree** — `git worktree remove --force ../dex-<n>-slug`

## Build requirements

| Requirement | Why |
|-------------|-----|
| Go 1.22+ | generics, `log/slog` |
| cgo + libsqlite3-dev | `mattn/go-sqlite3` (vec0 + FTS5) |
| `-tags sqlite_fts5` | BM25 full-text search; without it, reindex wipes the store |

Never `go install ./cmd/dex` without `-tags sqlite_fts5`. The safe command is always
`mooncake task install`.

## Semantic search while developing

dex indexes itself. After your first `mooncake task install`, run:

```sh
dex index .
dex serve .
```

Then use `search_semantic`, `ask`, `graph_*` inside Claude Code to navigate
the codebase. See [docs/READING_ORDER.md](docs/READING_ORDER.md) for where
to start.

## Code conventions

- **Semantic search before grep** — query `dex` (MCP tools or `dex context`)
  before reading broadly.
- **No hand-rolled config parsers** — config is YAML via `yaml.v3`
  (`.dex/config.yml`).
- **Canonical log attributes** — use `logx.Path`, `logx.Phase`, `logx.Count`,
  `logx.Model`, `logx.DurMS` instead of bare strings. See
  [docs/observability.md](docs/observability.md).
- **No broad refactors in scope** — a bug fix is a bug fix, not a cleanup pass.
- **Comments only when the why is non-obvious** — no docstrings, no "what" comments.
- **Test via mooncake** — `mooncake task test` runs with the correct build tags.

## PR checklist

- [ ] `mooncake task ci` passes (includes vet, fmt, vuln, arch, budget, dupl)
- [ ] New behaviour has a test
- [ ] Commit message is Conventional Commits
- [ ] PR description states what changed and why (not just what)
- [ ] Issue is claimed and will be closed when merged

## Issue hygiene

- One issue per logical unit. Split multi-part work into multiple issues.
- State transitions: `todo → in_progress → done` (or `unclaim` if dropped).
- Comment at real checkpoints, not just at open/close.
- Never leave an issue `in_progress` abandoned — unclaim it so others can pick it up.
