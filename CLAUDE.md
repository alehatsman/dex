# dex

Local semantic-search MCP server for Claude Code (Go). Indexes a repo and
serves `search_semantic` / `ask` / `search_symbol` / `graph_*` over MCP.

## Workflow — track work as moongit issues (mgit)

Prereq: the repo has a `moongit` remote (code mirror) — or `MOONGIT_SERVER`
points at the server. Export your **own** `MOONGIT_TOKEN` (`mgt_…`); the
token's name is your identity in every claim/comment, so never share one.

1. **Survey:** `mgit issue list --state todo,in_progress`.
2. **Plan as issues** — one issue per unit of work, the plan in the body. Split
   multi-part work into multiple issues:
   `mgit issue create --title "<t>" --body "<plan>"`.
3. **Claim before coding:** `mgit issue claim <n> --state in_progress`. Never
   work an issue already `in_progress` under another identity.
4. **Report progress** at real checkpoints: `mgit issue comment <n> --body "…"`.
5. **Close out** when merged + verified: `mgit issue set-state <n> done`
   (`mgit issue unclaim <n>` if you drop it).

No code without an owned issue. Worktrees for non-trivial changes; never
auto-push; ask before merge; conventional branches/commits. Merge
fast-forward only: `mgit pr merge <n> --ff-only` — the bare command
defaults to a merge commit (moongit #360), so always pass `--ff-only`;
if `main` advanced, rebase your branch and re-merge.

## Build / test — always via mooncake task

**Never use bare `go build`, `go test`, or `go install` directly** — the
project requires `-tags sqlite_fts5` for FTS5 support (mattn/go-sqlite3)
and without it store tests panic and the built binary drops BM25 search.
The canonical commands:

```
mooncake task install   # build + install to ~/bin/dex
mooncake task test      # go test -tags sqlite_fts5 ./...
mooncake task ci-fast   # pre-commit gate (build + test + vet + fmt-check)
mooncake task ci        # full pre-push gate
```

`GO_TAGS=sqlite_fts5` is declared once in `tasks.yml:37` and threaded into
every task automatically. Do not repeat it at call sites.
