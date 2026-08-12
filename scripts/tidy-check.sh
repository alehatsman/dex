#!/usr/bin/env bash
# tidy-check — fail if go.mod / go.sum are not tidy (#153).
#
# The shared goq/ci-fast (pre-commit) already checks tidy drift, but the full
# goq/ci (pre-push, what moongit CI runs) does not — so drift that skips the
# commit hook reaches CI unguarded. This closes that hole and mechanically
# enforces the "minimal deps" principle. Restores go.mod/go.sum on drift so a
# failing run leaves the tree clean (mirrors go-quality's fast.sh).
set -euo pipefail

cd "$(git rev-parse --show-toplevel 2>/dev/null || pwd)"

go mod tidy
if ! git diff --quiet go.mod go.sum; then
  echo "  ✗ go.mod / go.sum out of sync — run 'go mod tidy' and commit the result" >&2
  git checkout -- go.mod go.sum 2>/dev/null || true
  exit 1
fi
echo "  ✓ go.mod / go.sum are tidy"
