---
id: cargo-topology
status: draft
owners: [aleh]
covers:
  - "internal/graph/sitter_rust.go"
  - "internal/graph/resolve/cargo.go"
  - "internal/graphquery/packages.go"      # importTargetPath (Rust target metadata)
  - "internal/retrieve/assembler.go"        # projectOfFor
  - "internal/mcp/server_graph_deps.go"     # packageGraph --level project gate
extends:
  - specs/ask-project-topology.md   # #151 — project rollup for JS/TS
  - specs/module-resolution.md      # #127 — resolved-internal import DAG
---

# Cargo workspace crate topology (#162)

## Goal

On a Cargo **workspace** repo, make dex report the crate import DAG the same way
it reports Go package and JS/TS project DAGs: `dex index` counts real crates,
`graph packages` returns the module-level import DAG, and `graph packages
--level project` / `ask(intent=package_topology)` return the crate-level rollup.
Today all three return empty (`graph: 0 packages`) on a Cargo workspace, while
the Rust call graph works fine.

## Root cause (two independent gaps)

1. **Import targets never resolve.** `graphquery.importTargetPath` returns
   `metaString("target")` for every non-Go language, but the Rust extractor emits
   `NodeImport` nodes with no `target` metadata. So `BuildPackageGraph` computes
   `to == ""` for every Rust import edge and drops it — Rust has **never** had a
   package DAG, single-crate or workspace.
2. **Workspace layout loses crate identity.** `rustPackagePath` only strips a
   leading `src/`. A workspace lays files out as `crates/<name>/src/…`, which
   never starts with `src/`, so `crates/foo-core/src/lib.rs` becomes package
   `crates::foo-core::src::lib` and a cross-crate `use foo_core::Bar` (import path
   `foo_core::…`) matches none of those synthetic paths. Crate identity is lost,
   so even after gap #1 the rollup has nothing to roll up to.

Single-crate repos hit only gap #1 (their `src/` strip is correct).

## Design

Mirror the JS/TS design (#127 resolved-internal imports → #151 project rollup),
Rust-flavoured. Three surgical pieces; the call-graph extraction path is
untouched except that workspace package paths become *consistent with* the
`use`-path namespace (a net improvement to cross-crate call resolution, never a
regression — see Edge cases).

### 1. `internal/graph/resolve/cargo.go` — Cargo workspace model

A small, dependency-free reader (line-oriented, same spirit as the package's
existing `jsonc.go`) for the two facts we need:

```go
// CargoWorkspace maps workspace member dirs to crate identifiers and answers
// "which crate owns this file / package path".
type CargoWorkspace struct { /* unexported: []member{dir, crate} */ }

// LoadCargo parses root Cargo.toml [workspace].members (explicit paths and
// trailing "/*" globs), then each member's Cargo.toml [package].name. The Rust
// crate *identifier* is the package name with '-' → '_' (Cargo's own rule); a
// member whose name is workspace-inherited falls back to its dir basename.
func LoadCargo(root string) *CargoWorkspace

// IsCargoWorkspaceRoot reports whether root/Cargo.toml has a [workspace] table.
func IsCargoWorkspaceRoot(root string) bool

// CrateForFile maps a project-root-relative .rs path to (crate, memberDir, ok).
func (w *CargoWorkspace) CrateForFile(relPath string) (string, string, bool)

// ProjectOf maps a Rust package path (`crate::mod::sub`) to its crate — the
// first `::` segment when it names a known crate, else "".
func (w *CargoWorkspace) ProjectOf(pkgPath string) string
```

Plus a shared dispatcher so the two rollup call sites don't branch on workspace
kind:

```go
// ProjectOfForRoot returns root's package→project mapper and whether root is any
// recognized workspace (JS/TS or Cargo). nil,false for a non-workspace repo.
func ProjectOfForRoot(root string) (func(string) string, bool)
```

### 2. `internal/graph/sitter_rust.go` — crate-aware package paths + import targets

- `Init(root)` loads `e.cargo = resolve.LoadCargo(root)` (nil when not a Cargo
  workspace — zero behaviour change off-workspace).
- New method `packagePathFor(relPath)`: when `e.cargo` owns the file
  (`CrateForFile`), derive a crate-relative path — `crates/foo-core/src/a/b.rs`
  → `foo_core::a::b`, `…/src/lib.rs` (or `main.rs`) → crate root `foo_core`,
  non-`src/` member files → `foo_core::<rel>`. Otherwise fall back to the
  existing `rustPackagePath(relPath)` (single-crate / non-workspace — unchanged).
  `ProcessFile` calls this instead of the free function.
- In `Finalize`, after all package nodes exist, resolve each Rust `NodeImport`'s
  `Metadata["target"]` to the **longest package-path prefix that is a real
  package** in the project (drop the imported symbol's trailing segments). This
  is the Rust analogue of the JS resolver writing `target`, done once with the
  full package set in hand. `use foo_core::a::Thing` → target `foo_core::a` when
  that module is indexed, else `foo_core`. `importTargetPath` (unchanged) then
  reads it, so `BuildPackageGraph` sees internal Rust edges.

### 3. Rollup gate — reuse `ProjectOfForRoot`

- `retrieve.projectOfFor(req)`: for `IntentPackageTopology`, return
  `resolve.ProjectOfForRoot(req.ProjectRoot)`'s mapper (nil off-workspace). Drops
  the JS-only `IsWorkspaceRoot` check.
- `mcp.packageGraph`: gate `--level project` on `ProjectOfForRoot`'s `ok` and
  feed its mapper to `BuildProjectGraph`. Go / non-workspace still `no-graph`;
  the JS/TS path is byte-for-byte the same mapper as before.

No wire-shape change: crate nodes are `GraphNode{Kind:"package"}` whose ID is the
crate identifier, exactly like a JS project node.

## Edge cases

- **Call-graph safety.** In a workspace the old package paths
  (`crates::foo-core::src::…`) were internally consistent but never matched the
  `use`-path namespace, so cross-module/cross-crate calls silently under-resolved.
  New paths are consistent *and* match `use` paths → resolution improves. Same-pkg
  calls (the validated `codex_catalog_entries`→`parse_transcript`) are unaffected.
  Single-crate repos take the fallback path — identical to today.
- **`mod` targets.** `emitModDecl`'s crate-root test (`currentPkg != "lib" &&
  != "main"`) still holds: a workspace crate root is `foo_core` (≠ lib/main) so
  `mod a;` → `foo_core::a`, matching the file's package. Intra-crate edges roll
  up to a crate self-edge and are dropped (`from == to`).
- **Name inheritance / globs.** `[package] name.workspace = true` → dir basename;
  `members = ["crates/*"]` → expand to immediate subdirs with a Cargo.toml.
- **Non-crate import targets.** A `use` of an external crate (no member match)
  resolves to no internal package prefix → `target` stays unset → dropped as
  external, exactly like a bare npm import.

## Validation

- Unit: `resolve` Cargo loader (explicit + glob members, hyphen→underscore,
  ProjectOf first-segment); `sitter_rust` package-path derivation (workspace vs
  single-crate) and Finalize target resolution; a fixture workspace →
  `BuildPackageGraph` non-empty + `BuildProjectGraph` yields the crate DAG.
- Regression: existing Rust call-graph tests stay green; Go and JS/TS package /
  project graphs unchanged (`#154` gate still `no-graph` on Go).
- Live: reindex the Cargo workspace → `graph: N crates`, `ask(package_topology)`
  returns the crate DAG, `look` call-graph still resolves. `mooncake task ci`.
