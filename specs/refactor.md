---
id: refactor
status: living
last_verified: c4b4bdc
owners: [aleh]
covers:
  - "internal/refactor/**"
  - "cmd/dex/refactor.go"
  - "internal/mcp/server_refactor.go"
---
# Refactor

## Intent

Plan a type-precise rename of a Go symbol and return byte-exact edit triples for
the host agent to apply. dex is read-only by design (#551): `refactor` never
writes files. v1 supports one operation: `rename_symbol` for Go. Other ops
(change_signature, extract_method, inline, move) and a gopls compile-precheck
are deferred to v2.

## Behavior

- WHEN `op` is not provided or is empty, defaults to `rename_symbol`.
- WHEN `op` is anything other than `rename_symbol`, returns `status=error` immediately.
- WHEN `symbol` or `to` is empty, returns `status=error`.
- WHEN `to` is not a valid Go identifier or is a Go keyword (`token.IsIdentifier`
  / `token.IsKeyword`), returns `status=error` with a descriptive hint.
- WHEN the project root has no `go.mod`, returns `status=unsupported-language`
  with a hint that v1 is Go-only.
- WHEN the project root is not supplied, resolves it from the server's working
  directory via `proj.Resolve`.

**Symbol resolution** (`resolveTarget` in `internal/refactor/rename.go`):

- IF the symbol matches `(*T).M` or `(T).M` (receiver-qualified method), resolves
  the method on that type via `lookupMethod`; the type name may be
  package-tail-qualified (e.g. `(*mcp.Server).Run` → type `Server`, method `Run`).
- IF the symbol contains a `.` but is not a receiver form, tries
  package-tail-qualified lookup first (`pkg.Name`), then method lookup
  (`Type.Method`), then field lookup (`Type.Field`).
- IF the symbol is a bare name, searches top-level scopes of all loaded packages;
  if found in exactly one package returns it, if found in multiple returns
  `status=ambiguous` with a hint to qualify the name.
- IF a bare name matches no top-level declarations, falls back to a
  method-by-name search across all named types; same ambiguity guard applies.
- IF nothing matches, returns `status=not-found`.

**Package load**: `go/packages` loads `./...` under `projectRoot` with
`NeedName|NeedFiles|NeedSyntax|NeedTypes|NeedTypesInfo|NeedImports|NeedDeps|NeedModule`.
No pre-built index is needed — source is read directly at call time.

**Edit collection** (`collectEdits`):

- Walks `TypesInfo.Defs` and `TypesInfo.Uses` across all loaded packages.
- Matches by `types.Object` identity, so a method rename touches only that exact
  method, not same-named methods on other types.
- Deduplicates by `filename:offset` so shared packages traversed more than once
  don't produce duplicate edits.
- Each `EditTriple` carries: `path` (project-relative, slash-separated), `start_byte`
  (0-based byte offset), `end_byte`, `replacement` (the new name), `line`
  (1-based, for display only).
- Edits are sorted by `(path, start_byte)`. The agent MUST apply edits
  highest-offset-first within each file to keep prior offsets valid.

**Scope collision check**: WHEN `to` already exists in the target's scope
(struct field/method scope for fields and methods; package scope otherwise),
appends a human-readable entry to `Warnings` but still emits the edits. This is
a warning, not an error; compile verification is deferred to v2.

**Etag / stale-plan detection**:

- After building the edit set, hashes the current content of every touched file
  (SHA256 over sorted relative path + file bytes, truncated to 16 hex chars).
- Returns the hash as `etag` in the output.
- IF the caller passes a non-empty `etag` that does not match the computed hash,
  returns `status=stale` with a hint to re-plan before applying.

**Output** (`RefactorOutput`):

```
status   string         // ok | unsupported-language | not-found | ambiguous | stale | error
hint     string         // human-readable explanation on any non-ok status
project  string         // resolved project root
op       string         // "rename_symbol"
from     string         // original symbol
to       string         // new identifier
object   string         // resolved description, e.g. "method (*Server).Run"
edits    []EditTriple   // sorted by (path, start_byte)
files    int            // distinct files touched
etag     string         // 16-char hex hash of touched files' current content
warnings []string       // non-fatal issues (scope collision, etc.)
```

**CLI** (`dex refactor`):

- Positional args: `[<path>] <symbol> <to>` — path defaults to cwd.
- `--op` flag (default `rename_symbol`); `--etag` for stale-plan guard;
  `--format text|json` (default `text`).
- Text output prints `rename X → Y (desc) — N edit(s) across M file(s) [etag ...]`,
  then per-file, per-edit `L<n> bytes <s>-<e> → <replacement>`, followed by the
  reminder "apply highest-offset-first per file".
- Non-ok status prints to stderr; exits 0 (the status field carries the error,
  not the process exit code).

## Non-goals

- **Does not write files.** All file mutation is the host agent's responsibility.
- **Go-only in v1.** Non-Go projects, mixed-language projects, and symbols that
  can only be resolved via non-Go tooling return `unsupported-language`.
- **No cross-module renames.** The load is scoped to `./...` under `projectRoot`;
  callers in other modules are not found or updated.
- **No compile verification.** Post-rename compilation is not checked; a gopls
  compile-precheck is deferred to v2. The scope-collision warning is the only
  pre-check.
- **No API-break warning.** Renaming an exported symbol does not trigger a
  dedicated warning beyond the scope-collision check.
- **No ops beyond rename_symbol.** change_signature, extract_method, inline, and
  move are deferred.

## Checklist

- [x] `to` validated as a Go identifier before any package load
- [x] `go.mod` presence checked before any package load
- [x] Symbol resolution covers receiver-qualified, package-tail-qualified, bare names,
      methods, and struct fields
- [x] Type-resolved matching: only the exact `types.Object` identity is renamed
- [x] Dedup by `filename:offset` guards against shared-package double-counting
- [x] Edits sorted `(path, start_byte)` — agent applies highest-offset-first per file
- [x] Etag computed and stale-plan check enforced before returning `ok`
- [x] Scope-collision emitted as warning, not error
- [x] `unsupported-language` returned for non-Go roots (no `go.mod`)
- [x] Verified against the code by the verify workflow (flip to `living`)
