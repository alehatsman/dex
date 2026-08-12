---
id: unresolved-imports
status: living
last_verified: 7bf8cb6
owners: [aleh]
covers:
  - "internal/graph/resolve/resolve.go"
  - "internal/graph/sitter_jsts.go"
  - "internal/store/store_graph.go"
  - "internal/mcp/verbs.go"
---
# Honest representation of unresolved JS/TS imports (#130)

## Intent

#127 Phase 1 resolves non-relative JS/TS specifiers to real files. What it does
**not** — and by design **cannot** — resolve is a specifier whose target only
exists at build stage. The canonical case, found live on `acme-frontend`:
`@acme/common/Uuid` (589 imports) is a public export declared in
`package.json` `exports["./Uuid"]` → `build/Uuid.js`, a file a `vite` plugin
writes in `closeBundle` as a compatibility shim re-exporting `src/UuidCodec.ts`
(dodging a case-insensitive-FS collision with the npm `uuid` package). The
`Uuid → UuidCodec` binding exists in **no source file** — only in generated
output produced by arbitrary build-plugin code.

Resolving it to `src/UuidCodec.ts` would inject a **build-stage fact into a
source graph**: it would give `UuidCodec` 589 cross-package callers that do not
exist in source, and would go stale the moment `src` is edited without a rebuild
— a confident-wrong edge, the worst outcome for an agent. So the fix is **not**
to resolve harder; it is to represent the unresolved state **honestly** and
surface it where an agent actually reads it.

Today an unresolved import is a *silent third state*: `{"language":"ts"}` with
neither `target` nor `external` — indistinguishable from "not processed" or
"bug". And `trace(UuidCodec, callers)` reports its 5 in-package callers as if
complete; the existing name-grep recall sweep greps `UuidCodec`, but the 589
hidden call sites say `@acme/common/Uuid`, so it finds none. The undercount is
invisible.

## Scope

1. **Explicit status (Step 1).** Every JS/TS import carries a definite
   resolution state — never a silent blank. Unresolved imports gain
   `Metadata["unresolved"]=true` plus a `reason` the classifier already knows.
2. **Quantified recall (Step 2).** `trace`/`impact` on a non-Go target surface
   the count of known-unresolved imports **into the target symbol's package**,
   with their specifiers and a grep hint — turning a silent undercount into an
   honest, actionable one.

Explicitly out of scope (→ #604 LSP lane): reading `build/` output, interpreting
build plugins, following re-export chains, resolving the concrete source symbol
behind a build-mediated export. Confirmed unresolvable from source. No fabricated
`target` is ever emitted for a build-mediated export.

## Interfaces

### `internal/graph/resolve` — candidate provenance

```go
// Origin records where a specifier's candidates came from, so an unresolved
// import can be labeled honestly (an alias target vs a workspace subpath with
// no source file). Alias wins over Workspace, matching Candidates' precedence.
type Origin int
const (
    OriginExternal Origin = iota // no match — bare npm dependency
    OriginAlias                  // matched a tsconfig/jsconfig path alias
    OriginWorkspace              // matched a workspace package (bare or subpath)
)

type Classification struct {
    Candidates []string // same slice Candidates returns
    Origin     Origin
    PkgDir     string   // matched workspace package dir (Origin==OriginWorkspace)
}

// Classify is Candidates plus provenance. Candidates delegates to it.
func (w *Workspace) Classify(specifier string) Classification
```

`Candidates(spec)` becomes `w.Classify(spec).Candidates` — no behavior change.

### `sitter_jsts.go` — reason on the unresolved class

Before any resolution attempt, `classifySpecifier` short-circuits a **static-asset
import** — a specifier ending in a stylesheet (`.css/.scss/.sass/.less/.styl`),
image (`.svg/.png/.jpg/.jpeg/.gif/.webp/.avif/.ico/.bmp`) or font
(`.woff/.woff2/.ttf/.eot/.otf`) extension (a `?query`/`#hash` suffix is trimmed
first) — to `specExternal`. A bundler imports these as resources, not code
modules; from the code graph they are external, never an unresolved code edge.
This is what keeps a workspace-prefixed asset (`@scope/ui/theme.css`) from
reaching the `OriginWorkspace` branch and acquiring a `pkg_dir`, which would
otherwise surface a stylesheet as a phantom caller in `unresolved_inbound` (#157).

`classifySpecifier` gains a third return, `reason string`, non-empty only for
the `specUnresolved` class:
- `relative` — a `./x` / `../x` that probed no indexed file.
- `alias-unindexed` — `OriginAlias`, candidates present, none indexed (e.g. a
  tsconfig alias to a `.d.ts` stub that isn't chunk-indexed).
- `workspace-subpath` — `OriginWorkspace`, candidates present, none indexed: a
  workspace-package subpath with no source file. This is where a build-mediated
  export (the `Uuid` shim) and a genuine typo/dead import both land; the two are
  not distinguishable from source, so one honest label covers them.

`annotateImports` stamps unresolved imports:
- `Metadata["unresolved"] = true` (always).
- `Metadata["reason"] = <reason>`.
- `Metadata["pkg_dir"] = <Classification.PkgDir>` for `workspace-subpath` only —
  the join key Step 2 uses. Node `Name`/`QualifiedName` stay the raw specifier.

`internal`/`external` annotation is unchanged. The three states are now
mutually exclusive and total for every JS/TS import.

### `internal/store` — unresolved-inbound query

```go
type UnresolvedInbound struct {
    Specifier string `json:"specifier"` // e.g. "@acme/common/Uuid"
    Count     int    `json:"count"`
}

// UnresolvedInboundForFile returns workspace-subpath-unresolved import
// specifiers whose target package dir (Metadata["pkg_dir"]) is a path-prefix of
// file, grouped and counted, most-frequent first. These are known import edges
// into the file's package that name-based recall cannot see (e.g. build-mediated
// exports). Empty when none — no cost surfaced.
func (s *Store) UnresolvedInboundForFile(ctx context.Context, file string, limit int) ([]UnresolvedInbound, error)
```

Query keys on `json_extract(metadata_json,'$.pkg_dir')` (present only on
`workspace-subpath` imports) with `:file LIKE pkg_dir || '/%'`.

### `internal/mcp/verbs.go` — surfacing

`TraceOutput` gains `UnresolvedInbound []store.UnresolvedInbound
json:"unresolved_inbound,omitempty"`. In `traceVerb`:
- `callers`/`callees` and `impact`, when the result has a non-Go target, look up
  `UnresolvedInboundForFile` for each distinct target file, merge/sum by
  specifier, and set `UnresolvedInbound` + append a hint:
  `"N unresolved import(s) into this symbol's package (build-mediated /
  workspace subpath) — name-based recall cannot see them; grep the specifier(s)
  to confirm"`.
- Recall is already `partial` on non-Go targets; this augments it with specifics.
- Zero unresolved-inbound ⇒ field omitted, no hint change.

Surfaced only for **callers** and **impact** (incoming analyses — these edges are
potential hidden callers); never for **callees** (outgoing), where inbound imports
are irrelevant.

The capability is an *optional* interface `unresolvedInbounder`, type-asserted
inside the fold — implemented on `*Server` and its `projectScoped` wrapper, so no
`toolSurface` widening and remote/maintenance/test surfaces skip gracefully. The
merge + hint are factored into `mergeUnresolvedInbound` / `UnresolvedInboundHint`,
shared with the CLI.

**CLI parity.** `dex trace` uses its own path (`CallEdgeOutput` / `ImpactOutput`,
not `TraceOutput`), so those two structs gain the same `unresolved_inbound`
(`omitempty` — omitted, hence no shape change, on the MCP `graph_callers` /
`graph_impact` tools). The CLI callers/impact runners populate it via
`(*Server).UnresolvedInboundForTargets` and print it in text mode (shown even at
zero resolved callers, where it matters most), so terminal `dex trace` matches
the MCP verb.

## Edge cases

- **No unresolved imports** (clean Go repo, or fully-resolved JS/TS): every path
  no-ops; `unresolved_inbound` omitted; existing output byte-identical.
- **Typo import** (`@acme/common/Nope`): labeled `workspace-subpath` like a
  build shim. Honest — dex can't tell them apart from source, and both are "a
  workspace subpath with no source file." The grep hint lets the agent decide.
- **pkg_dir prefix safety**: match uses `pkg_dir || '/'` so `packages/acme`
  never matches `packages/acme-common/...`. Trailing slash is mandatory.
- **Non-workspace unresolved** (relative miss, alias-unindexed): carries
  `unresolved`+`reason` but **no** `pkg_dir`, so it never appears in a Step 2
  join — those aren't package-inbound edges. Status is still explicit.
- **Go targets**: `hasNonGoTarget` already gates the augment; Go traces are
  untouched (Go resolves its own imports; no unresolved leaves).

## Validation

- `resolve` golden tests: `Classify` returns `OriginAlias` for an alias match,
  `OriginWorkspace` + `PkgDir` for a workspace subpath, `OriginExternal` for a
  bare dep; `Candidates` output unchanged.
- `internal/graph` fixture: extend `ts_workspace` with a workspace-subpath import
  that has **no** source file; assert the import node gets
  `unresolved=true, reason="workspace-subpath", pkg_dir=<pkg>` and that a bare
  dep stays `external`, a resolved subpath stays `target`.
- `store` test: seed workspace-subpath unresolved imports + a symbol file in the
  same package; `UnresolvedInboundForFile` returns the specifier with the right
  count; a file in a *different* package returns none; prefix-safety asserted.
- `verbs` test: a `trace callers` on a non-Go symbol whose package has
  unresolved-inbound imports populates `UnresolvedInbound` and the hint; a Go
  symbol and a clean package do not.
- `mooncake task ci` green.
- Acceptance (manual, `acme-frontend`): `@acme/common/Uuid` imports carry
  `unresolved=true, reason=workspace-subpath, pkg_dir=packages/acme-common`;
  `trace UuidCodec callers` reports its in-package callers **plus**
  `unresolved_inbound: [{@acme/common/Uuid, 589}]` and the grep hint;
  `UuidCodec` gains **no** fabricated cross-package caller edge.

## Refs
- #130 (this work); #127 / `specs/module-resolution.md` (the resolver it refines);
  #604 (LSP lane — where build-artifact / re-export-chain resolution is deferred).
- Evidence: `packages/acme-common/vite.config.ts` `uuidCompatibilityShim`
  (`closeBundle` writes `build/Uuid.js` = `export * from './UuidCodec.js'`).
</content>
