---
title: Workspace-project rollup for JS/TS monorepos (#127 Phase 3)
status: draft
covers:
  - internal/graph/resolve/resolve.go
  - internal/graphquery/packages.go
  - internal/graph/graph.go
  - internal/mcp/server_graph_deps.go
  - cmd/dex/graph.go
extends:
  - specs/module-resolution.md
  - specs/reexport-binding.md
---

# Workspace-project rollup (#127 Phase 3)

## Goal

Give a JS/TS monorepo a **project-level** dependency view above the per-file
module graph, and make the reindex "N packages" counter report real projects.

Today `NodePackage` for JS/TS is **per source file** (`packages/acme-ui/src/Button`
is a "package"). A monorepo has ~30 real workspace projects (`@acme/ui`,
`@acme/common`, …) but the package graph has 3000+ per-file nodes, and reindex
reports `graph: 0 packages` (only `ExtractGo` increments the counter; the sitter
path never contributes it).

Chosen shape (decided with the user): a **query-time rollup**, not a first-class
`NodeProject` graph node. No schema change, no new node kind, no reindex-format
change — the project DAG is *derived* from the module DAG we already build, using
workspace boundaries the resolver already knows. Contained and reversible.

## Non-goals

- **First-class `NodeProject` nodes** — rejected: would touch the store and every
  node consumer (search/brief/centrality/dedup) to exclude a new kind. The rollup
  is a view, not persisted graph.
- **Impact project rollup** — folding a symbol's blast radius to project
  granularity is a natural follow-up but out of this phase; keep it bounded.
- **The chunk-density-cap skip** (large `acme-api` files dropped at cap=500) —
  separate recall gap, filed independently.
- **Cross-language projects** — a project is a JS/TS workspace package (package.json
  name + dir). Go keeps its existing dir-level package model unchanged.

## Design

### 1. Expose projects from the resolver

`resolve.Workspace` already retains `packages []pkgEntry{name, dir, …}`. Add:

```go
type Project struct { Name, Dir string } // Dir is project-relative, slash

func (w *Workspace) Projects() []Project // all workspace packages, dir desc
```

Sorted by `len(Dir)` descending so the **longest** (most specific) dir prefix
wins when a nested package sits inside another's tree.

### 2. Path → project mapper + rollup (graphquery, pure)

```go
// ProjectOf returns the name of the workspace project owning module path `p`
// (longest Dir prefix on a path boundary), or "" when none owns it.
func ProjectOf(p string, projects []Project) string

// BuildProjectGraph rolls the module import DAG up to projects: every endpoint
// is mapped via ProjectOf, intra-project edges (from==to) are dropped, edges are
// deduped, degrees + PageRank recomputed. Modules under no project are dropped.
// Pure — projects are injected, no I/O — so it unit-tests against a hand view.
func BuildProjectGraph(view *View, projects []Project) PackageGraph
```

`BuildProjectGraph` reuses `BuildPackageGraph`'s edge-walk and sort/dedup: it is
the same aggregation with `ProjectOf(endpoint)` substituted for the raw module
path and a project-boundary drop. `PackageStat.Package` then holds a project name
(`@acme/ui`); `IsMain` is false (projects aren't executables). Prefix match is
boundary-safe: `p == Dir || strings.HasPrefix(p, Dir+"/")`.

### 3. Fix the "N packages" counter

In `graph.Run`, after `ExtractSitter`, count **distinct projects owning ≥1
emitted JS/TS `NodePackage`** and add to `result.Packages`:

```go
projects := resolve.Load(root).Projects()
seen := map[string]struct{}{}
for _, n := range sitterRes.Nodes {
    if n.Kind == NodePackage && n.Language() != "go" {
        if proj := graphquery.ProjectOf(n.PackagePath, projects); proj != "" {
            seen[proj] = struct{}{}
        }
    }
}
result.Packages += len(seen)
```

Computed **once** at Run level (not per language extractor) so js+ts extractors
don't double-count the same workspace. Go's `res.Packages` is untouched; a Go
repo reports the same number, a JS/TS repo now reports real projects, a mixed repo
reports the sum. Only projects with indexed modules count — tooling-only
package.json (root, config packages with no source) don't inflate it.

### 4. Expose the project view

`PackageGraphInput` gains `Level string` (`"module"` default | `"project"`).
When `project`, `packageGraph` routes to `BuildProjectGraph(view,
resolve.Load(p.Root).Projects())` instead of `BuildPackageGraph(view)`; the wire
output shape is unchanged (still `PackageStat`/`PackageImport`, `Package` now a
project name). CLI: `dex graph packages --level project`. Default stays module —
no behavior change for existing callers.

## Validation

- Unit (`resolve`): `Projects()` returns the fixture's packages, dir-desc.
- Unit (`graphquery`): `ProjectOf` — boundary safety (`packages/acme` vs
  `packages/acme-common`), longest-prefix, unowned → ""; `BuildProjectGraph`
  on a hand view — module edges roll up, intra-project dropped, degrees/PageRank
  correct, deterministic order.
- Unit (counter): a JS/TS fixture reports its real project count (not 0, not
  per-file); a Go fixture is unchanged; mixed sums.
- Live (acme-frontend): `dex graph packages --level project` yields ~30
  projects with `@acme/common` / `@acme/ui` high in-degree (foundation
  packages); reindex reports a real `N packages` (was 0). `mooncake task ci` green.
