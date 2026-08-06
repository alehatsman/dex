---
id: module-resolution
status: living
last_verified: 7d7aa4a
owners: [aleh]
covers:
  - "internal/graph/resolve/**"
  - "internal/graph/sitter_jsts.go"
  - "internal/graph/sitter_jsts_tags.go"
  - "internal/graphquery/packages.go"
---
# Module resolution (JS/TS workspace)

## Intent

On a real monorepo the single most valuable thing a code-intel tool can report —
*what depends on what* — is exactly what dex is blind to for JavaScript/TypeScript.
The tree-sitter extractor resolves only **relative** import specifiers (`./foo`,
`../bar`); every non-relative specifier — workspace packages (`@bright/*`), path
aliases (`@/*`), bare npm deps (`react`, `@mui/*`) — is stored verbatim as a
dangling import-leaf and never linked to a target. Measured on the 27-project
`bright-frontend` Rush monorepo: 30,809 import edges, 100% dead-ending at synthetic
leaves; 0 of 3,416 call edges cross a package boundary; the package-import DAG is
Go-only by construction so no JS/TS dependency graph exists at all.

This spec defines a **workspace-aware module resolver** that maps non-relative
JS/TS specifiers to real project files using the same config the ecosystem's
dependency tools use (dependency-cruiser, madge, Nx, Turborepo, rev-dep):
`package.json` workspace names + `tsconfig`/`jsconfig` path aliases + entry-point
fields. It is **not a type checker** and does **no `node_modules` resolution** —
config-driven, deterministic, offline, fits the tree-sitter architecture. It is the
"ecosystem-standard middle": ~90% of the monorepo dependency value at a fraction of
LSP cost, upgradeable later toward the LSP precision lane (#604) for exactness.

This is **Phase 1** of #127 (dependency graph). Cross-package *symbol* binding
(fixing `trace`/impact under-recall) is Phase 2; first-class workspace-project
nodes + rollups are Phase 3 — both out of scope here and specced separately.

## Scope

In scope (Phase 1):
1. A pure `internal/graph/resolve` package that loads workspace config from the
   repo root and, given a non-relative specifier, returns candidate
   project-relative, extension-free module paths in priority order.
2. Wiring it into `jstsBase.resolveModuleSpecifier` so non-relative specifiers
   resolve to real project files (probed against the indexed file set).
3. Annotating each `NodeImport` in `Finalize` with its resolution outcome
   (`target` = resolved internal module path, or `external` = true for a bare
   dep) so downstream consumers stop treating every import as an opaque leaf.
4. Generalizing `BuildPackageGraph` so the package-import DAG covers any
   resolved-internal import graph (Go **and** JS/TS), not Go alone.

Out of scope: type inference / re-export chains / conditional-`exports`
condition selection / dynamic `import()` (best-effort or deferred to #604);
cross-*language* resolution; Phase 2 symbol binding; Phase 3 project nodes;
Python/Rust/Java resolvers (Python & Go already resolve their own imports).

## Interfaces

### `internal/graph/resolve`

```go
// Workspace resolves non-relative JS/TS import specifiers to project-relative,
// extension-free module-path candidates, using package.json workspace names and
// tsconfig/jsconfig path aliases. No type checking, no node_modules resolution:
// a specifier that matches neither an alias nor a workspace package is external.
type Workspace struct { /* unexported */ }

// Load scans root for package.json (name → dir, entry candidates) and
// tsconfig.json/jsconfig.json (compilerOptions.paths + baseUrl, following
// "extends"), skipping node_modules/.git and dot-dirs. Malformed or missing
// config yields an empty Workspace — Load never returns an error that could
// fail an index; the resolver simply resolves nothing.
func Load(root string) *Workspace

// Candidates returns project-relative, extension-free module-path candidates for
// a non-relative specifier, most-specific first. Empty slice ⇒ treat as external
// (bare npm dep). The caller probes each candidate against the indexed file set
// (knownFiles) and takes the first hit; nothing here touches the graph or DB.
func (w *Workspace) Candidates(specifier string) []string
```

Resolution order inside `Candidates`:
1. **tsconfig/jsconfig path aliases** — longest/most-specific pattern first. A
   pattern `@/*` → `["src/*"]` with `baseUrl:"."` maps `@/util/x` →
   `src/util/x`. Exact (non-`*`) patterns match the whole specifier.
2. **Workspace package names** — longest name first. `@bright/common` →
   `packages/bright-common`; a bare package import (`@bright/common`) yields the
   package's *entry candidates* (from `exports`/`main`/`module`/`types`, plus
   `<dir>/index` and `<dir>/src/index` probes); a subpath import
   (`@bright/common/String`) yields `<dir>/String` (and `<dir>/src/String`).
3. Otherwise empty (external).

All candidates are returned extension-free and project-root-relative with forward
slashes, matching the `knownFiles` key space (`<path minus ext>`).

### `jstsBase` wiring (`sitter_jsts.go`)

- New field `workspace *resolve.Workspace`, built **once** at the top of
  `Finalize` (`e.projectRoot` is set; the resolver reads only disk config, so it
  does not depend on `knownFiles` being complete).
- `resolveModuleSpecifier(specifier, fromFile)` keeps its signature and its
  relative-path branch verbatim. For a non-relative specifier it now consults
  `workspace.Candidates`, probing each candidate against `knownFiles` with the
  **same** exact / ext-strip / `/index` fallback used for relative paths
  (factored into a shared `probeKnown` helper). First hit ⇒ the resolved module
  path; no hit ⇒ the raw specifier (unchanged behavior).
- `classifySpecifier(specifier, fromFile) (target string, class)` — a thin
  wrapper used only by import-node annotation: `internal` (target resolved to a
  `knownFiles` entry), `external` (non-relative, `Candidates` empty), or
  `unresolved` (matched a candidate but no indexed file / relative miss).

### Import-node annotation (`Finalize`)

After the existing import-map resolution loop, iterate `e.nodes` once; for each
`NodeImport`, recover the importing file via `knownFiles[node.PackagePath]`,
classify its `QualifiedName`, and set metadata:
- `internal` → `Metadata["target"] = <resolved module path>`
- `external` → `Metadata["external"] = true`
- `unresolved` → unchanged (stays a bare leaf)

The import node's `Name`/`QualifiedName` stay the **raw specifier** (display and
node identity unchanged); resolution rides in metadata only. `Metadata["target"]`
survives serialization (`MarshalMetadata`) and is readable post-load via
`MetadataJSON`.

### `BuildPackageGraph` generalization (`graphquery/packages.go`)

- `internal` set = **all** `NodePackage` paths (Go per-package + tree-sitter
  per-file module), not just Go.
- Per import edge, the target is computed by language:
  - Go node (`Language()=="go"`) → `dst.QualifiedName` (the resolved import
    path, unchanged).
  - tree-sitter node → `dst`'s `Metadata["target"]` (resolved-internal only);
    absent ⇒ the edge is dropped (external / unresolved never became a target).
- `IsMain` stays Go-only (package clause `== "main"`); a tree-sitter file merely
  named `main.ts` is not an executable.
- Everything else (dedup, degrees, PageRank, deterministic sort) is unchanged.

## Edge cases

- **No config at all** (plain single-package repo): `Load` returns an empty
  Workspace; `Candidates` always empty; every non-relative specifier is external
  — identical to today's behavior. No regression for non-monorepo repos.
- **Bare npm deps** (`react`, `@mui/material`): no workspace/alias match ⇒
  external, labeled, never a DAG node. (No `node_modules` walk.)
- **Alias vs workspace collision**: aliases win (checked first) — they are the
  more specific, intentional redirect.
- **Workspace-wide resolution**: `tsconfig` `paths` are resolved relative to the
  config's own `baseUrl` from the **repo root**, not per-package cwd — this is the
  documented dependency-cruiser pitfall (#862) and dex must avoid it. `extends`
  is followed; `references` is not required for path resolution in Phase 1.
- **Subpath with explicit extension** (`@bright/common/String.js` importing a
  `.ts` source): candidates are emitted ext-free after stripping a known JS/TS
  extension, so the `.ts` source still matches.
- **Candidate matched but file not indexed** (workspace package excluded by
  ignore rules): `unresolved` — no target node exists, so no edge; not mislabeled
  external.
- **Non-JS/TS graphs** (Go, Python, Rust, Java): untouched. Go's DAG output is
  byte-identical (its imports already carry resolved paths in `QualifiedName`;
  the `internal` set now also contains non-Go packages but Go import targets
  never collide with file-path module ids).

## Validation

- `go test -tags sqlite_fts5 ./internal/graph/resolve/...` — golden tests for
  `Candidates`: relative-passthrough (n/a — relative handled upstream), tsconfig
  `paths` (glob + exact + `baseUrl`), workspace bare-name, workspace subpath,
  `extends` chain, and external (empty). Workspace-wide-from-root guards the #862
  cwd pitfall.
- `internal/graph` extractor tests: a fixture monorepo (package.json name +
  tsconfig paths + a cross-package import) asserts the import node gains
  `Metadata["target"]` pointing at the real file, a bare dep gains
  `external:true`, and a relative import still resolves as before.
- `internal/graphquery` `TestBuildPackageGraph` is **rewritten**: the linked
  tree-sitter package pair (previously asserted *excluded*) now **appears** as a
  DAG edge via `Metadata["target"]`; Go packages still appear; an external
  import (no target, no package node) is still dropped; `IsMain` stays Go-only.
- Whole change gated by `mooncake task ci`.
- Acceptance (manual, on `bright-frontend`): the package-import DAG lists
  `apps/* → @bright/*` dependencies; `@bright/*` / `@/*` imports resolve to
  internal targets; `@mui/*` labeled external, not leaf. (`trace capitalize`
  cross-package recall is Phase 2, not asserted here.)

## Non-goals / precision boundary

Not a type checker. Re-export chains, conditional `exports`/`imports`
condition-name selection, dynamic `import()`, and computed member access are
best-effort; exactness is deferred to the LSP precision lane (#604). Phase 1
delivers the dependency **graph** (edges between modules/packages), not
cross-package **symbol** binding (Phase 2) or project-level nodes (Phase 3).

## Refs
- Root cause: `internal/graph/sitter_jsts.go` (`resolveModuleSpecifier`,
  `Finalize`); `internal/graphquery/packages.go` (`BuildPackageGraph`,
  `isGoPackageNode`).
- Issue #127 (this work); #74 / #126 (ignore/noise — orthogonal); #604 (LSP
  precision lane — the exactness upgrade this defers to).
- Prior art: dependency-cruiser #862 (per-cwd tsconfig-paths pitfall), rev-dep
  (workspace-wide config resolution, no type-check).
