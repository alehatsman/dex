---
title: ask(package_topology) — project rollup for JS/TS monorepos (#151)
status: draft
covers:
  - internal/retrieve/graph.go
  - internal/retrieve/assembler.go
extends:
  - specs/project-rollup.md   # #127 Phase 3 — BuildProjectGraph
  - specs/ask-merge.md         # #110 four-verb front door
---

# ask(package_topology) — project rollup (#151)

## Goal

Make `ask(intent=package_topology)` return the **workspace-project import DAG** on
JS/TS monorepos, the same structure `dex graph packages --level project` already
prints (23 projects / 90 edges on acme-frontend, PageRank-ranked). Today the
intent returns an empty graph + semantic-doc noise there; the fix
(`graphquery.BuildProjectGraph`, #127 Phase 3) exists but is unreachable from the
`ask` retrieve path.

## Root cause

`retrieve.EnrichGraph` → `packageTopology()` (graph.go) walks `NodePackage` nodes
+ `EdgeImports`, seeded from `packagesFromPaths(semHits)`. Two JS/TS-specific
failures compound:

1. "packages" are per-file **modules**, not workspace projects (the #127 problem).
2. The semantic lane surfaces **docs** (`rush.json`, `apps/*/packaging/recipe.rb`)
   that map to **no package nodes** → `packagesFromPaths` is empty → the topology
   seeds nothing.

Go is unaffected: real `NodePackage` nodes + import edges → the module lane works
(dex repo returns ~50 edges).

## Design

Give `package_topology` a **project-rollup lane**, preferred for a genuine JS/TS
workspace and gated so a Go repo never reaches it.

- **New `resolve.IsWorkspaceRoot(root) bool`** — a ROOT-only check for a top-level
  workspace manifest (`pnpm-workspace.yaml`, `rush.json`, `lerna.json`, or a
  root `package.json` with a `workspaces` field). This is the crux: `resolve.Load`
  walks the *whole tree* for `package.json`, so it discovers buried JS/TS **test
  fixtures** in a Go repo (dex's own `graph/resolve/testdata` → 3 bogus projects).
  Gating on a real workspace root — not on `Load` returning any packages — is what
  separates a JS/TS monorepo from a fixture-bearing Go repo. Verified: acme-frontend
  has `rush.json`; dex has only `go.mod`.
- **Thread `projectOf func(string) string` into `EnrichGraph`** as its last param,
  injected by `projectOfFor(req)` in the assembler — built **only** for
  `package_topology` **and only when `IsWorkspaceRoot(req.ProjectRoot)`**, as
  `resolve.Load(root).ProjectOf` (the same mapper `graph packages --level project`
  uses). `nil` otherwise, which is the Go / non-workspace path. Keeps `retrieve`
  pure: the mapper is injected, mirroring `BuildProjectGraph`'s own signature; the
  common read intents never pay for the disk walk.
- **New lane `projectTopology(projectOf)`:** call
  `graphquery.BuildProjectGraph(view, projectOf)`; if it emits nodes, convert its
  `PackageStat`/`PackageImport` into `GraphNode{ID:project, Kind:"package"}` /
  `GraphEdge{From,To,Kind:"imports"}` and return `true`. Project names *are* the
  compact IDs — no view lookup, no wire-shape change. Node order is preserved
  (in-degree desc), so the `MaxGraphNodes`/`MaxGraphEdges` caps keep the most
  load-bearing foundation projects.
- **Dispatch (whole-DAG, three tiers — #190):** `GraphLanePackageTopology` tries,
  in order, `projectTopology()` (JS/TS workspace rollup, gated on a workspace
  root), then `moduleTopology()` (the module import DAG, every internal package
  ranked by fan-in — parity with `dex graph packages`), then `packageTopology()`
  (the old neighborhood lane, only if the repo has no indexed package graph at
  all). Every tier returns the *whole* DAG, so the answer is **stable regardless
  of which files the semantic lane surfaced** — the defect that forced a
  bottom-up read onto the CLI. The root gate keeps JS/TS fixtures out of the
  project tier; `BuildPackageGraph`'s testdata/vendor filter (#181) keeps them
  out of the module tier.
- **Ranking on the wire (#190):** both topology tiers project each
  `PackageStat`'s `InDegree`/`OutDegree`/`PageRank` onto `GraphNode`
  (`in_degree`/`out_degree`/`page_rank`, all `omitempty`). Only these lanes
  populate them, so every other intent's nodes keep the lean `{id,kind}` shape.
  This is the fan-in profile a bottom-up architecture read needs — no second
  CLI call.

Non-goals (filed as follow-ups on #151):
- Suppressing doc-window inlining for the topology intent (the ~15 KB waste is the
  #113 inline pass, separable).

## Validation

- **Unit (retrieve):** a JS/TS-style module view + a map-backed `projectOf` →
  `package_topology` emits project nodes + rolled-up edges (intra-project edges
  dropped). A Go-style view with `projectOf → ""` → unchanged module fallback
  (same output as today). `projectOf == nil` → module fallback.
- **Regression:** the 9 existing `EnrichGraph` test callers pass `nil` and stay green.
- **Unit (module DAG, #190):** a Go-style view (a→b, a→c, b→c) with `projectOf ==
  nil` and *no* semHits → `package_topology` emits all 3 packages ranked by fan-in
  (c first, in=2), each carrying in/out-degree + PageRank, buried testdata
  fixtures excluded. Proves the answer is independent of the semantic neighborhood.
- **Live (acme-frontend):** `ask(package_topology)` surfaces the 23-project DAG
  in `graph.edges` (`@acme/common`/`@acme/build-helpers` high in-degree).
- **Live (dex, Go, #190):** `ask(package_topology)` returns the whole module DAG
  ranked by fan-in with `in_degree`/`page_rank` per node — parity with
  `dex graph packages`, no CLI drop-out.
- `mooncake task ci` green.
