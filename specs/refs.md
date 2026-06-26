---
id: refs
status: living
last_verified: 68c08d2
owners: [aleh]
covers:
  - "cmd/dex/refs.go"
  - "internal/mcp/server_refs.go"
  - "internal/symbols/symbols.go"
---
# Refs

## Intent

`refs` answers type-precise relationship questions about Go symbols — all definition
and use sites, concrete implementors of an interface, the interfaces a type satisfies,
and the types that extend or embed a given type. Unlike `trace`, which reads a
pre-built static call graph stored in the index, `refs` invokes `go/types` at query
time (`golang.org/x/tools/go/packages` loads `./...`) and requires no prior `dex index`
run. Unlike `symbol-search`, which does an exact name lookup over indexed chunks,
`refs` computes type-checked relationships that span the full module type graph.

## Behavior

- WHEN called, `refs` runs `packages.Load("./...")` against the project root and
  type-checks the entire module on demand — no index is read or written.
- WHERE the project root does not contain a `go.mod`, `refs` returns status
  `unsupported-language` with a hint that v1 is Go-only and requires a module root.
- WHEN `action` is `references`, `refs` returns every definition site (`kind: "def"`)
  and use site (`kind: "use"`) of the symbol across the full module, deduplicated
  by file/line/kind.
- WHEN `action` is `implementations`, `refs` returns every concrete (non-interface)
  named type that implements the target interface, each with `kind: "implementor"`.
  If the target is not an interface, it returns status `error` with a hint to use
  `subtypes` for embedding structs. An empty interface returns `ok` with a hint
  that every type qualifies.
- WHEN `action` is `supertypes`, behavior depends on the target kind:
  - For an interface: returns the directly embedded interfaces (`kind: "super"`).
  - For a concrete type: returns the non-empty interfaces within the module that
    the type (or pointer to it) satisfies (`kind: "super"`).
- WHEN `action` is `subtypes`, behavior depends on the target kind:
  - For an interface: returns types (concrete or interface) that implement or
    extend it (`kind: "sub"`).
  - For a concrete type: returns structs that embed it as an anonymous field
    (`kind: "sub"`).
- IF `action` is empty, it defaults to `references`.
- IF `action` is not one of `references`, `implementations`, `supertypes`,
  `subtypes`, `refs` returns status `error` with a list of valid actions.
- WHEN a symbol cannot be resolved in any loaded package, `refs` returns
  status `not-found` with a hint naming the symbol.
- WHERE a symbol is ambiguous (same bare name in multiple packages without a
  package qualifier), the first match wins; callers should qualify with the
  package tail (`mcp.NewServer`) to disambiguate.
- WHEN resolving a symbol, three forms are accepted:
  - Bare name: `Foo` — matches any exported name in the module.
  - Receiver-qualified: `(*Server).Run` or `(Server).Run` — matches a method on
    the named type; the receiver may be package-tail-qualified (`(*mcp.Server).Run`).
  - Package-tail-qualified: `mcp.NewServer` — restricts search to packages whose
    path tail or declared name matches the prefix.
- WHILE each `Site` carries `path` (relative to project root, slash-separated),
  `line` (1-indexed), and `kind`, it does not carry a signature, body snippet, or
  column number.
- WHEN results are returned, they are sorted by path then by line (ascending);
  the sort is stable and deterministic across runs.
- WHERE the MCP tool surface is configured, `refs` is gated behind `DEX_EXPERT`
  (the analysis/power lane, alongside `cohort`, `smells`, and `routes`); it is
  absent from the default everyday tool set.
- WHEN called as a CLI verb (`dex refs`), `refs` accepts an optional leading
  project path argument, then `<action> <symbol>`, and supports `--format text`
  (default) or `--format json`.
- WHEN `--format text` is used and status is not `ok`, `refs` writes the status
  and hint to stderr and exits without error.
- WHEN `--format json` is used, the full `RefsOutput` struct is written to stdout
  as indented JSON regardless of status.
- WHILE `refs` loads type information, it never writes files, modifies the index,
  or alters any persistent state (`ReadOnlyHint: true`).

## Non-goals

- **Index dependency.** `refs` never reads or writes the dex chunk/graph index.
  It is a pure `go/types` client; the index is the `storage` spec's domain.
- **Non-Go languages.** v1 is Go-only. Any project root without `go.mod` returns
  `unsupported-language`. Multi-language support is deferred.
- **Column-level precision.** Sites carry file and line only, not column or
  surrounding context.
- **Interface coverage gaps.** Finding near-miss implementors (types that
  implement most but not all of an interface) is `cohort`'s job. `refs`
  `implementations` returns only exact satisfiers.
- **Call graph traversal.** Callers and callees of a function are `trace`'s
  domain (pre-built graph). `refs references` returns all use sites but does
  not filter to call sites specifically.
- **Cross-module queries.** `refs` loads only `./...` under the project root;
  external dependency types are visible for type-checking but are not returned
  as result sites.
- **Signature or body content.** Result sites do not include code snippets;
  callers use `read(path, mode=lines:N)` to fetch context.

## Checklist

- [x] Four actions: `references` (def+use), `implementations` (concrete implementors),
      `supertypes` (embedded interfaces or satisfied interfaces), `subtypes`
      (implementing types or embedding structs)
- [x] Default action when omitted: `references`
- [x] Symbol resolution: bare name, `(*Recv).Method`, `pkg.Name` — all three forms
- [x] Go-only v1: no `go.mod` → `unsupported-language`
- [x] Per-site output: `path` (relative, slash-separated), `line`, `kind`
      (`def` / `use` / `implementor` / `super` / `sub`)
- [x] Status codes: `ok`, `unsupported-language`, `not-found`, `error`
- [x] Results sorted by path then line; deterministic across runs
- [x] MCP tool in DEX_EXPERT lane; absent from default surface
- [x] CLI verb `dex refs [path] <action> <symbol>` with `--format text|json`
- [x] Read-only: `ReadOnlyHint: true`; no index reads or writes
- [x] Verified against the code by the verify workflow (flip to `living`)
