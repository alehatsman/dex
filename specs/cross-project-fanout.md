# Cross-project fan-out query (#221)

## Goal
One `query()` call runs the same read across N already-indexed local
projects and returns labeled, merged results — no new backend, no shared
store, no cross-project graph joins.

## Scope
MCP only for this slice (`query` has no CLI subcommand today — CLI verbs are
ask/read/grep/etc. under `cmd/dex/verbs.go`; adding a `dex query` CLI entry
point is out of scope, follow-up if wanted).

## Interface
`QueryInput` (internal/mcp/query.go) gets one new optional field:

```go
ProjectRoots []string `json:"project_roots,omitempty" jsonschema:"run this query independently across these already-indexed project roots and merge labeled results; omit for single-project behavior (project_root)"`
```

- Empty/absent (default): byte-for-byte current single-project behavior.
- Non-empty: `project_root` is ignored (or rejected — see edge cases), the
  same `QueryInput` (same `Input`/`Kind`/`Want`/pipe string/etc.) runs once
  per root, resolved via the existing `s.resolveProject`.

## Discovery helper (reused by both sides)
Move `knownProjectRoots` off `cmd/dex` (main_manage.go:281) into
`internal/proj` as `proj.KnownRoots(ctx, indexBaseDir string) ([]string, error)`
so `internal/mcp` can special-case `project_roots: ["all"]` without an
import from `internal/mcp` → `cmd/dex` (wrong direction) or duplicating the
~30 lines of store-open/meta-read logic. `cmd/dex/main_manage.go` calls the
moved function; behavior unchanged (same store.Open, same skip-and-warn on
missing `project_root` meta).

`"all"` as a sentinel inside `project_roots` (i.e. `project_roots: ["all"]`)
expands to `proj.KnownRoots(ctx, s.IndexDir)`. Explicit paths in
`project_roots` are NOT resolved through discovery — they're passed straight
to `resolveProject` same as today's single `project_root`, so an unindexed
path surfaces as this project's own `no-index` status, not a fan-out error.

## Execution
- Fan out with a bounded goroutine pool (existing repo doesn't have one on
  the query path — start simple: one goroutine per root, no cap, since N is
  "locally indexed projects", realistically single digits to low tens).
- Each root runs the FULL existing single-lane or pipe path unchanged
  (`dispatchSingle`/`runPipe`) with that root substituted for
  `in.ProjectRoot`. No new lane logic — this is orchestration only.
- Per-project failure (not indexed, resolve error, lane error) does not
  abort the fan-out: captured as a per-project status, sibling projects'
  results still return (issue's "degrades cleanly" success criterion).

## Output
`QueryOutput` gets one new optional field, populated only when
`project_roots` was set:

```go
type QueryFanout struct {
    Root   string              `json:"root"`
    Status string              `json:"status"` // "ok" | "no-index" | "error"
    Error  string              `json:"error,omitempty"`
    Result *QueryFanoutResult  `json:"result,omitempty"`
}
```
`QueryOutput.Fanout []QueryFanout `json:"fanout,omitempty"``

`QueryFanoutResult` is a cycle-free projection of `QueryOutput` (same fields
minus `Fanout` itself — the MCP SDK's schema generator panics on a
self-referential type, and a per-project sub-query never recurses into
another fan-out since `dispatchFanout` always clears `ProjectRoots` before
rerunning). It carries the full envelope (status/hint/route/result/refs/
trust/cost/next) per project, richer than the originally-drafted bare
`QueryResult` pointer.

The top-level `Result`/`Route` stay empty when fanning out — `Fanout` is the
whole answer, one entry per root, in the order `project_roots` was given
(explicit ordering, not completion order — goroutines write into a
pre-sized slice by index, not appended, so the response is deterministic
even though execution is concurrent).

## Capping
`in.K` (existing "max results per lane" field) caps PER PROJECT, same as it
does today for a single project — no new total-across-projects cap field.
Rationale: the issue's "cap total like every other lane" is satisfied by
each per-project sub-result already being capped by the lane's own K; a
second global cap would double-encode the same knob for a feature scoped to
"single digits to low tens" of projects. If real usage shows unbounded
project counts, revisit as a follow-up.

## Non-goals (from issue, restated)
- No merged/deduped/re-ranked single result list across projects.
- No cross-project symbol resolution (a symbol lane run per-project stays
  scoped to that project's graph).
- No new persistent store or shared index.

## Edge cases
- `project_roots` set AND `project_root` also set → `project_root` ignored,
  warning-free (fanout is the more specific ask).
- `project_roots: []` (empty slice, not omitted) → treated same as omitted
  (single-project path), since Go's zero value for `omitempty` slice is nil
  and an explicit `[]` is indistinguishable in intent from omission here.
- A root in `project_roots` that isn't a git worktree / doesn't resolve →
  per-project `status: "error"`, not a hard failure of the whole call.
- `project_roots: ["all"]` with zero known projects → `Fanout: []`, top-level
  `status` still success (empty is not an error).
- Pipe input (`"pkg:store | callers"`) with `project_roots` set → each root
  runs the full pipe independently; no cross-project pipe stages (consistent
  with the non-goal on cross-project graph joins).

## Validation
- Unit: `queryVerb` with 2+ fake/temp indexed projects (existing test
  fixtures under `internal/mcp/*_test.go` already spin up temp stores) —
  assert per-root isolation, order-preservation, and one deliberately
  broken root not poisoning the others.
- `proj.KnownRoots` move: existing `cmd/dex` reindex --all test coverage
  (if any) must still pass unchanged after the move.
- `mooncake task ci-fast` before commit.
