---
title: Re-export binding for JS/TS barrels (#127 Phase 2)
status: draft
covers:
  - internal/graph/sitter_jsts.go
  - internal/graph/sitter_jsts_tags.go
  - internal/graph/sitter_ts.go
extends:
  - specs/module-resolution.md
  - specs/unresolved-imports.md
---

# Re-export binding (#127 Phase 2)

## Goal

Bind a cross-package call whose import path lands on a **barrel `index.ts`** that
only *re-exports* the definition, so `trace`/`impact` stop under-reporting.

Phase 1 already binds **direct/subpath** cross-package calls: an import specifier
resolves to a module path, and `e.symbols` is keyed by that same module-path
space, so `import { capitalize } from '@bright/common/String'` →
`symbolIn("packages/bright-common/src/String", "capitalize")` already works
(verified live: `trace capitalize` → 12 cross-package callers).

The remaining gap is the **barrel**: `import { Button } from '@bright/ui'`
resolves to `packages/bright-ui/src/index`, whose `index.ts` says
`export * from './Button'` — it *defines* nothing named `Button`, so
`symbolIn(index, "Button")` misses and the call edge is dropped.

## Scope — the three re-export shapes that actually occur

Measured across `packages/*/src/index.ts` in bright-frontend:

| Shape | Count | Powers | Example |
|-------|-------|--------|---------|
| `export * from './x'` | 75 | `@bright/ui` (671 imp), `@bright/api` (527) | star |
| `export * as NS from './x'` | 26 | `@bright/common` barrel | namespace |
| `export { a, b as c } from './x'` | 16 (+6 `type`) | misc | named |

All three are in scope. They are the complete re-export vocabulary of these
barrels; delivering only star would leave `@bright/common`'s namespace barrel
(`String.capitalize()`) dark.

## Design

### Capture (per-file, during the query walk)

The tags query already enumerates `import_statement`. Add `export_statement`
capture. For each top-level `export_statement` that has a `source` field (the
`from './x'` clause), record into a per-module re-export table keyed by the
exporting module's packagePath. Raw specifier + fromFile are stored verbatim —
resolution is deferred to `Finalize` (same reason as imports: `knownFiles` is
incomplete mid-walk).

Three record kinds, distinguished by the `export_statement` children:

- **star** — `export * from './x'` (no `export_clause`, no `namespace_export`):
  append raw specifier to `reExports[mod].stars`.
- **namespace** — `export * as NS from './x'` (`namespace_export` child):
  `reExports[mod].namespaces[NS] = rawSpecifier`.
- **named** — `export { a, b as c } from './x'` (`export_clause` child, `source`
  present): for each `export_specifier`, `reExports[mod].named[exportedName] =
  {spec: rawSpecifier, orig: localName}` where `exportedName` is the alias if
  present (`b as c` ⇒ exported `c`, orig `b`), else the bare name.

`export type { … } from` and `export type * from` are captured identically —
type-only re-exports still carry symbols the graph binds; the extra recall is
harmless and the grammar tags them the same (a leading `type` keyword child).

A plain local `export` (`export function foo`, `export { foo }` with **no**
`from`) is NOT a re-export — those already land in `e.symbols` via the existing
declaration emit. Only `export … from …` (source present) is captured here.

### Resolve (in `Finalize`, before `resolveCall`)

Mirror the existing import-specifier resolution loop: walk every module's
re-export table and rewrite each raw specifier to its packagePath via
`resolveModuleSpecifier(spec, fromFile)`. A specifier that doesn't resolve to a
known project file stays as the returned raw string and simply never matches a
symbol bucket — no error, no fabricated edge.

### Consult (in `resolveCall`)

Introduce `resolveExport(pkg, name string, depth int) string`:

```
if id := e.symbolIn(pkg, name); id != "" { return id }   // local def wins
if depth >= maxReExportDepth { return "" }                 // cycle/fan-out cap
re := e.reExports[pkg]
if r, ok := re.named[name]; ok {                           // named re-export
    if id := e.resolveExport(r.mod, r.orig, depth+1); id != "" { return id }
}
for _, starMod := range re.stars {                         // star re-exports
    if id := e.resolveExport(starMod, name, depth+1); id != "" { return id }
}
return ""
```

Replace the two `symbolIn(fi.pkg, fi.name)` sites in `resolveCall`'s **bare**
case and `resolveTypeMethod` with `resolveExport(fi.pkg, fi.name, 0)`. Direct
same-package `symbolIn` calls (`callerPkg` lookups) are unchanged — a call to a
locally-defined symbol never needs re-export following.

**Namespace** handling lives in the **attr** case. Today `String.capitalize()`
with `import { String } from '@bright/common'` reaches
`fi = fromImports["String"] = {pkg: index, name: "String"}` and tries
`symbolIn(index, "String.capitalize")` — a miss. Add: if `re.namespaces[fi.name]`
names a target module `m`, resolve `resolveExport(m, tail[0], 0)`. This binds
`String.capitalize` to `packages/bright-common/src/String`'s `capitalize`.

### Cycle & fan-out safety

`maxReExportDepth = 8`. Barrels legitimately nest (`index` → `Foo/index` →
`Foo/Bar`), but 8 hops is far beyond real depth; the cap bounds both genuine
cycles (`a` re-exports `b` re-exports `a`) and pathological `export *` fan-out.
First hit wins across stars — deterministic given stable capture order (source
line order, preserved by the query walk).

## Non-goals

- **Conditional / `package.json` `exports` maps** — deferred (#604 precision lane).
- **Wildcard `export *` name *collision* arbitration** — if two star targets both
  export `X`, first-in-source-order wins. Real barrels don't collide; exactness
  is a type-checker's job.
- **Build-mediated re-exports** (the `@bright/common/Uuid` vite shim) — stays
  unresolved + honestly surfaced per [[specs/unresolved-imports.md]]. Re-export
  binding only follows re-exports that exist *in source*.
- **Default re-export** (`export { default } from './x'`) — rare in these
  barrels; not captured. Can be added later if a case appears.

## Validation

- Unit: extend `sitter_ts_workspace_test.go` — a barrel `index.ts` with all three
  shapes re-exporting sibling modules; assert cross-package call edges bind
  through it (star, named, namespace) and that a cycle terminates.
- Live (bright-frontend): a symbol exported only via `export *` from `@bright/ui`
  (e.g. a `Button`-like) gains its `apps/` callers in `trace --dir callers`;
  count cross-package `calls` edges before/after; confirm no explosion in edge
  count from fan-out (sanity bound).
- `mooncake task ci` green.
