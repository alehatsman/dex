# Subpath `exports` resolution (issue #130)

Follow-up to #127. Extends `internal/graph/resolve` so `@pkg/subpath` imports
resolve through a package's `package.json` `exports` **subpath map**, including
the dominant real-world case where the exported target is a *compiled artifact*
that re-exports a differently-named source.

## Motivating case

`@bright/common/Uuid` is imported 589× across bright-frontend and was 100%
unresolved. The chain:

```
package.json  "exports": { "./Uuid": { "import": "./build/Uuid.js", ... } }
build/Uuid.js  ->  export * from './UuidCodec.js';   (a re-export barrel)
src/UuidCodec.ts  (the actual source; NO src/Uuid.ts exists)
```

Survey of bright-frontend: 9 packages, 22 subpath keys, **20 targets under
`build/`**, **0** with a direct `build/→src/` sibling. So a pure path rewrite
(`build/Uuid.js` → `src/Uuid.ts`) resolves nothing — `src/Uuid.ts` doesn't
exist. The name only connects through the barrel. Resolution therefore requires
following one re-export hop in the compiled artifact.

## Mechanism (all work at `Load`; `Classify` stays pure)

For each package whose `exports` is an object, parse every **exact** subpath key
(`"./X"`, i.e. keys other than `"."` and without a `*`). For each, resolve the
condition object to its target string (reusing `firstStringLeaf`) and derive
source candidates, in priority order:

1. **Direct** — the target itself, cleaned (ext-free, project-relative).
2. **build→src retarget** — if the target's first path segment under the package
   dir is a build dir (`build`, `dist`, `lib`, `out`, `es`, `esm`, `cjs`), emit
   the path with that segment replaced by `src`, and also with it removed.
3. **One-hop barrel follow** — if the target file exists on disk and is a *pure
   re-export barrel* (small file, every statement an `export … from '…'`), read
   each re-exported specifier, resolve it relative to the artifact's directory,
   apply the same build→src retarget, and emit as a candidate. This is what
   resolves `./Uuid` → `src/UuidCodec`.

Results are stored per package as `subpaths map[string][]string` (key = the
subpath without the leading `./`). `Classify`'s subpath branch prepends
`subpaths[sub]` ahead of the existing generic `<dir>/<sub>` + `<dir>/src/<sub>`
fallback, so exact-export candidates win but nothing is lost when a package has
no `exports`.

**Star patterns** (`"./*"`) are *not* pre-resolved here: their path rewrite
(`./build/*.js` → `src/<sub>`) already coincides with the generic
`<dir>/src/<sub>` fallback, and a barrel follow would need a per-query disk read.
They keep the existing behavior.

## Bounds & safety

- FS reads happen only at `Load`, only on files **named as exports targets**
  (never a directory walk), and the barrel follow is a single hop.
- A file qualifies as a barrel only when it is `< 2 KB`, `< 24` statements, and
  **every** non-comment statement is a `export … from` re-export — so a large
  compiled/minified module is never mistaken for a barrel.
- Degrades cleanly: no `exports`, artifact absent (unbuilt checkout), or a
  non-barrel target → falls back to the generic subpath candidates. No regression
  for non-monorepo repos or packages without subpath exports.
- `Classify`/`Candidates` remain pure (no FS/DB/graph access after `Load`),
  preserving the package invariant.

## Validation

- `go test -tags sqlite_fts5 ./internal/graph/resolve/...` — golden cases:
  exact subpath → direct src, build→src retarget, barrel-follow to a
  differently-named source, and graceful fallback when the artifact is absent.
- Extractor-level testdata: a `bright-common`-shaped fixture with a `build/`
  barrel asserts the subpath import gains `Metadata["target"]` = the real source.
- Manual acceptance on bright-frontend: `@bright/common/Uuid` moves from
  unresolved → resolved (was 589 misses); reindex + `trace` shows the callers.

## Non-goals

Not a type checker. Multi-hop re-export chains, condition-name selection beyond
first-leaf, and star-pattern barrel following stay best-effort / deferred to the
LSP precision lane (#604). This closes the single dominant subpath-export miss
class, not arbitrary compiled-artifact indirection.
