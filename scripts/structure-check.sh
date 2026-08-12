#!/usr/bin/env bash
# structure-check — monotonic ratchet for structural code-quality metrics (#153).
#
# The shared go-quality checks (budget-status, dupl, deadcode) are
# informational-only: they PRINT drift but never fail, so god files, over-cap
# complexity, clone pairs and dead code can grow forever. This gate freezes each
# count in benchmark/structure/baseline.json and fails the CI gate when any count
# GROWS — a one-way door. Counts may shrink (baseline auto-tightens on refresh),
# never grow. Same idiom as the retrieval evals' `dex bench --check baseline.json`.
#
# Metrics (production code only, mirroring the shared scripts):
#   god_files         files >CAP_GOD_LOC LOC        (budget-status.sh)
#   gocyclo_over      functions >CAP_GOCYCLO cyclo   (budget-status.sh)
#   dupl_pairs        clone pairs at threshold T     (dupl-report.sh)
#   deadcode_symbols  unreachable symbols from main  (deadcode ./cmd/dex)
#
# Usage:
#   scripts/structure-check.sh            # check; fail if any count grew
#   scripts/structure-check.sh --refresh  # regenerate baseline from current tree
#   scripts/structure-check.sh --ci       # missing tool is FATAL (default: warn+skip)
set -euo pipefail

CAP_GOCYCLO="${CAP_GOCYCLO:-35}"
CAP_GOD_LOC="${CAP_GOD_LOC:-500}"
DUPL_T="${DUPL_T:-100}"
GO_TAGS="${GO_TAGS:-sqlite_fts5}"

REFRESH=0
CI_MODE=0
for a in "$@"; do
  case "$a" in
    --refresh) REFRESH=1 ;;
    --ci)      CI_MODE=1 ;;
    *) echo "structure-check: unknown arg '$a'" >&2; exit 2 ;;
  esac
done

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
cd "$REPO_ROOT"
BASELINE="benchmark/structure/baseline.json"
GOBIN="$(go env GOPATH)/bin"

have() { command -v "$1" >/dev/null 2>&1 || [ -x "$GOBIN/$1" ]; }
run()  { if command -v "$1" >/dev/null 2>&1; then "$@"; else "$GOBIN/$1" "${@:2}"; fi; }

# missing_tool <name> — record a skipped metric; fatal in CI, warn otherwise.
SKIPPED=""
missing_tool() {
  if [ "$CI_MODE" = "1" ]; then
    echo "structure-check: $1 not installed (fatal in --ci) — run 'mooncake task install-tools'" >&2
    exit 1
  fi
  echo "structure-check: $1 not installed — skipping its metric (run 'mooncake task install-tools')" >&2
  SKIPPED="$SKIPPED $1"
}

# ---- collect offender IDENTITY lists (one metric per temp file) --------------
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT

# god_files: identity = path (LOC-agnostic so a stable god file isn't spurious).
find . -name '*.go' -not -name '*_test.go' \
     -not -path './vendor/*' -not -path './.git/*' -not -path './.claude/*' \
     -exec wc -l {} + 2>/dev/null \
  | awk -v cap="$CAP_GOD_LOC" '$1 > cap && $2 != "total" {print $2}' \
  | sort > "$tmp/god_files"

# gocyclo_over: identity = "pkg.func @ pos".
if have gocyclo; then
  run gocyclo -over "$CAP_GOCYCLO" . 2>/dev/null | grep -v '_test\.go' \
    | awk '{print $2"."$3" "$4}' | sort > "$tmp/gocyclo_over" || true
else
  missing_tool gocyclo
fi

# dupl_pairs: identity = sorted clone pair (collapses A→B / B→A duplicates).
if have dupl; then
  run dupl -threshold "$DUPL_T" -plumbing . 2>&1 | grep -v '_test\.go' \
    | python3 -c '
import sys
pairs = set()
for line in sys.stdin:
    p = line.strip().split(": duplicate of ")
    if len(p) == 2:
        pairs.add(" <--> ".join(sorted(p)))
for x in sorted(pairs):
    print(x)
' > "$tmp/dupl_pairs" || true
else
  missing_tool dupl
fi

# deadcode_symbols: identity = full "file:line:col: unreachable ..." line.
if have deadcode; then
  run deadcode -tags "$GO_TAGS" ./cmd/dex 2>/dev/null | sort > "$tmp/deadcode_symbols" || true
else
  missing_tool deadcode
fi

METRICS="god_files gocyclo_over dupl_pairs deadcode_symbols"

# ---- refresh: write baseline (only metrics we actually measured) -------------
if [ "$REFRESH" = "1" ]; then
  mkdir -p "$(dirname "$BASELINE")"
  python3 - "$BASELINE" "$tmp" $METRICS <<'PY'
import json, os, sys
out, tmp, metrics = sys.argv[1], sys.argv[2], sys.argv[3:]
data = {}
for m in metrics:
    f = os.path.join(tmp, m)
    if not os.path.exists(f):      # tool missing → leave metric out
        continue
    items = [l.rstrip("\n") for l in open(f) if l.strip()]
    data[m] = {"count": len(items), "items": items}
json.dump(data, open(out, "w"), indent=2, sort_keys=True)
open(out, "a").write("\n")
print(f"structure-check: baseline written to {out}")
for m in metrics:
    if m in data:
        print(f"  {m:<18} {data[m]['count']}")
PY
  exit 0
fi

# ---- check: fail if any measured count grew over baseline --------------------
if [ ! -f "$BASELINE" ]; then
  echo "structure-check: no baseline at $BASELINE — run 'mooncake task structure-refresh'" >&2
  exit 1
fi

python3 - "$BASELINE" "$tmp" $METRICS <<'PY'
import json, os, sys
base_path, tmp, metrics = sys.argv[1], sys.argv[2], sys.argv[3:]
base = json.load(open(base_path))
failed = False
for m in metrics:
    f = os.path.join(tmp, m)
    if not os.path.exists(f):          # skipped (tool missing, non-CI) → ignore
        continue
    cur = [l.rstrip("\n") for l in open(f) if l.strip()]
    b = base.get(m)
    if b is None:
        print(f"  ~ {m:<18} not in baseline (count={len(cur)}) — run structure-refresh")
        continue
    bl_count, bl_items = b["count"], set(b.get("items", []))
    if len(cur) > bl_count:
        failed = True
        new = [x for x in cur if x not in bl_items]
        print(f"  ✗ {m:<18} {bl_count} -> {len(cur)}  (+{len(cur)-bl_count})")
        for x in new:
            print(f"        NEW  {x}")
    elif len(cur) < bl_count:
        print(f"  ↓ {m:<18} {bl_count} -> {len(cur)}  (improved — run structure-refresh to lock in)")
    else:
        print(f"  ✓ {m:<18} {bl_count}")
if failed:
    print("\nstructure-check: structural metrics grew above baseline (ratchet). "
          "Reduce them, or if intentional run 'mooncake task structure-refresh'.")
    sys.exit(1)
print("\nstructure-check: all structural metrics within baseline")
PY
