#!/usr/bin/env bash
# Repo-wide gofmt gate (#677).
#
# The full CI gate's golangci-lint has no `formatters:` block and runs with
# `run.tests: false`, so it never flags unformatted code — and the pre-commit
# gofmt check only inspects STAGED files. A file formatted-then-edited without
# re-staging, or a commit made with `--no-verify`, slips past both. This check
# closes that gap: `gofmt -l` lists every file whose formatting differs from
# gofmt's canonical form, across the whole tree (test files included),
# independent of git staging. A non-empty list fails the gate.
#
# Cheap (sub-second), deterministic, and the exact inverse of `mooncake task
# fmt` (gofmt -w .).
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
cd "$REPO_ROOT"

dirty="$(gofmt -l . | grep -v '^\.claude/worktrees/' || true)"
if [ -n "$dirty" ]; then
  echo "gofmt: the following files are not formatted — run 'mooncake task fmt':" >&2
  echo "$dirty" | sed 's/^/  /' >&2
  exit 1
fi

echo "gofmt: all Go files formatted"
