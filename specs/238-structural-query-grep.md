# Spec #238: native tree-sitter query facet for the grep lane

## Goal

Add a structural-match facet to `search_grep` (tool `grep`) alongside the
existing RE2 facet: accept a tree-sitter query pattern and return matches
by AST shape instead of by text, for the languages dex already parses
structurally (Python, JS, TS, TSX, Rust, Java).

## Non-goals

- No new external dependency (settled by #214's spike: ast-grep has no Go
  binding; `smacker/go-tree-sitter`, already vendored, already ships the
  full Query API).
- No new DSL — the input is raw tree-sitter query syntax, the same
  language callers of `ast-grep`/`nvim-treesitter`/etc. already know.
- No cross-file resolution, no ranking/fusion — this is a grep-shaped
  primitive (exact matches back to caller), not a semantic lane.
- No parse-result caching across calls in this pass — every call reparses
  candidate files fresh, matching the cost model `internal/graph`'s
  extractor framework already accepts (see Current state).

## Current state (read from code)

**Grep lane** (`internal/mcp/server_grep.go`):
- `SearchGrepInput` (`:17-25`): `ProjectRoot`, `Pattern` (RE2), `Path`,
  `Ext`, `MaxResults` (default 50, max 200), `Context` (0-10), `Fixed`.
- `searchGrep` (`:52-159`): resolves project → `grepFileList` walks the
  ignore-filtered working tree (not the index) → trigram-narrows
  candidates by `Pattern` (`:96-105`, RE2-shaped, doesn't apply to a
  structural query) → reads each file, regexes line-by-line, caps at
  `MaxResults`, truncates any line >2000 bytes UTF-8-safely.
- Output: `GrepMatch{Path, Line, Content, Before, After}` in
  `SearchGrepOutput{Status, Hint, Project, Matches, Total, Truncated}`.
- Throttle: grep passes `isSearch=false` to `s.ld().Check(...)` (`:135`)
  with an explicit comment — the RE2 scan is deterministic/local/cheap
  enough that blocking after the fact saves nothing.
- **Four call sites share `SearchGrepInput`/the `searchGrep` capability**:
  `Server` (real impl), `projectScoped` (`http_mcp.go:163`), `noopSurface`
  (`noop_surface.go:95`), `remoteClient` (`remote.go:303`) — plus CLI
  (`cmd/dex/verbs.go:184-230`) and HTTP (`http.go:265`). A new input field
  touches all of them.

**Tree-sitter framework** (`internal/graph/sitter*.go`):
- `ExtractSitterWith` (`sitter.go:205-361`) walks once per Run
  (ignore-filtered, 1 MB file cap), builds one `*sitter.Parser` per
  extension lazily, calls `ParseCtx(ctx, nil, src)` **per file, every
  run**, closes the tree after use. **No parse cache exists anywhere in
  the codebase** — this is the accepted cost model to mirror, not a gap
  to fix here.
- Per-language grammar entry points: `python.GetLanguage()`
  (`sitter_python.go:124`), `java.GetLanguage()` (`sitter_java.go:107`),
  `rust.GetLanguage()` (`sitter_rust.go:120`), plus JS/TS/TSX split across
  `sitter_javascript.go`/`sitter_ts.go`/`sitter_jsts.go` (TSX is a
  distinct grammar/extractor from plain TS).
- None of the four language tags-query strings in-repo use `#eq?`/
  `#match?` today — there is no existing predicate-evaluation call site
  to copy.

**`smacker/go-tree-sitter` Query API**
(`bindings.go` in the vendored module, pinned in go.mod, no replace):
- `NewQuery(pattern, lang)` (`:738`) compiles a `.scm`-style query,
  **validates** predicate syntax (arity/type) for
  `eq?/not-eq?/match?/not-match?/set!/is?/is-not?` but does not evaluate
  any of them at compile time.
- `QueryCursor.Exec` + `NextMatch` (`:973,1020`) iterate raw matches
  (predicates NOT applied).
- `QueryCursor.FilterPredicates(match, source)` (`:1094-1190`) is the
  piece that evaluates predicates — but it only implements
  `eq?/not-eq?` (capture-vs-capture or capture-vs-literal) and
  `match?/not-match?` (regex against capture text). **`set!`, `is?`,
  `is-not?` are parsed but never enforced** (fall through the switch,
  silently no-op — a match with an unsupported predicate is accepted as
  if the predicate always passed). There is no `kind-eq?` predicate in
  this library at all.

This is the one place spec and library reality genuinely conflict with
what a naive reading of "structural pattern matching" implies: **not
every predicate a user writes will be enforced**. Decision below.

## Design

### Input surface

Extend `SearchGrepInput` with two new optional fields, additive to the
existing RE2 path (only one facet is active per call):

```go
Query string `json:"query,omitempty" jsonschema:"tree-sitter structural query (.scm syntax) — alternative to pattern; requires lang"`
Lang  string `json:"lang,omitempty"  jsonschema:"language for a structural query: python|javascript|typescript|tsx|rust|java"`
```

- `Query` set (non-empty) → structural facet; `Pattern` set → RE2 facet
  (existing behavior, untouched). Both set, or neither set → `"error"`
  status with a hint, same pattern as the existing `Pattern == ""` check
  (`:56-58`).
- `Query` without `Lang` → `"error"`: a tree-sitter query is grammar-
  specific, there's no cross-language default to infer.
- `Lang` maps to grammar + extension set exactly as the four/six existing
  extractors do (reuse `python.GetLanguage()` etc. — no new grammar
  imports). Unknown `Lang` value → `"error"` with the supported list in
  `Hint`.
- File discovery reuses `grepFileList`/`walkProjectFiles` filtered to the
  extensions implied by `Lang` (e.g. `Lang: "typescript"` → `.ts` only,
  `Lang: "tsx"` → `.tsx` only, matching the existing TS/TSX extractor
  split) — `Ext` input field still applies as an additional filter if
  given, same as today.
- No trigram narrowing for the structural path — trigram index is built
  over regex-shaped text patterns and doesn't map to `.scm` queries; skip
  straight to the ignore-filtered file list.

### Matching

Per candidate file (fresh `sitter.NewParser()` + `ParseCtx` per file, no
cache, matching `internal/graph`'s accepted cost model):

1. Parse source → `*sitter.Tree`.
2. `sitter.NewQuery([]byte(in.Query), lang)` — compile once per call (not
   per file), return `"error"` with the library's compile error as
   `Hint` on failure (bad query syntax is a caller mistake, not a system
   error).
3. Per file: `QueryCursor.Exec(q, root)`, loop `NextMatch()`, call
   `qc.FilterPredicates(m, src)` and check the returned match has
   captures (this is the step confirmed missing from every existing
   in-repo query call site — required here to make `#eq?`/`#match?`
   actually filter, not just parse).
4. **Predicate scope**: if `PredicatesForPattern` reports any predicate
   operator outside `{eq?, not-eq?, match?, not-match?}` (i.e. `set!`,
   `is?`, `is-not?`, or anything unrecognized), reject the query at
   compile time with `"error"` and a `Hint` naming the unsupported
   operator — do not silently accept a query whose predicate will be
   ignored. This trades a class of valid-but-unenforceable queries for
   never returning a false "match" a user reasonably read as filtered.
5. Map each surviving match's primary capture node to
   `GrepMatch{Path, Line: node.StartPoint().Row + 1, Content: <line text
   at that row, existing truncateMatchLine reused>, Before/After if
   `Context` requested}` — same output shape as RE2, so callers don't
   branch on facet.
6. Cap at `MaxResults`/set `Truncated` exactly as the RE2 path does.

### Cost / throttle

Parsing every candidate file with a CGO tree-sitter grammar is a
different cost class than `bytes`-level regex scanning (the rationale
the existing `isSearch=false` comment is scoped to). The structural facet
sets `isSearch=true` in the throttle check when `Query` is set — RE2
calls keep today's `isSearch=false` behavior unchanged. No separate
file-count cap is added in this pass: the ignore-filtered walk + 1 MB
file-size cap (mirrored from `ExtractSitterWith`) plus `MaxResults`
early-exit are judged sufficient for a first cut; revisit if real usage
shows otherwise.

### Wiring

Same four interfaces + CLI + HTTP as any `SearchGrepInput` change
(`Server`, `projectScoped`, `noopSurface`, `remoteClient`,
`cmd/dex/verbs.go`, `http.go`) — no new tool name, no new endpoint,
`grep` tool gains two optional fields.

### Tests

Mirror `server_grep_test.go`'s harness (`fakeEmbed`/`newServer`/
`t.TempDir()`, call `s.searchGrep` directly). New cases: one structural
match per language (Python/TS/TSX/Rust/Java) proving shape-match beats
regex on a representative "match by shape not by text" query per language
(the issue's own success bar — 3 minimum, one per language is stronger);
invalid query syntax → `"error"`; unsupported predicate operator →
`"error"` naming the operator; `Query` without `Lang` → `"error"`;
`Query` + `Pattern` both set → `"error"`; unknown `Lang` → `"error"`.

## Open questions for review

1. **Predicate rejection vs. silent-ignore**: spec picks reject-at-compile
   for `set!/is?/is-not?` rather than accepting-and-ignoring. Confirm
   this is the right call — the alternative (accept, ignore silently,
   like the vendor library itself does) is less surprising to anyone who
   already knows plain tree-sitter query semantics, but reintroduces the
   false-negative risk the spec is trying to avoid.
2. **`Lang` as a required explicit field** vs. inferring from `Ext`/file
   extensions found in `Path`: spec requires explicit `Lang` because a
   single query string isn't valid across grammars (e.g. Python's
   `call` node shape differs from JS's `call_expression`) — inferring
   from a mixed-extension directory walk would be ambiguous. Confirm.
3. Naming: `Query`/`Lang` field names, or something else
   (`StructuralQuery`, `TsQuery`) to make the facet switch more obvious
   in the schema doc string? Current pick favors brevity to match
   `Pattern`/`Ext`'s existing style.
