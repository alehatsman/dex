---
id: ignore
status: living
last_verified: 621894f
owners: [aleh]
covers:
  - "internal/ignore/**"
---
# Ignore

## Intent

Ignore is dex's single chokepoint for deciding which files become index input.
Two questions meet here: what is *excluded* (gitignore-style patterns layered
from defaults, the repo's own ignore files, and dex config) and what is
*included* (an opt-in allow-list — dex indexes nothing until a project declares
one). On top of that gate the indexer narrows to chunkable content via an
extension/basename allowlist and drops files that are actually binary or that
carry a literal secret. Centralizing all of this in one matcher means the
indexer, the watch daemon, and the graph extractors inherit identical
file-selection rules. This spec covers the selection rules themselves; applying
them in the pipeline is the indexing spec.

## Behavior

- WHERE no `index.include` is declared in `.dex/config.yml`, the matcher
  selects no file at all — indexing is opt-in, so dex never embeds a tree the
  owner didn't ask for, and callers warn so an empty index isn't silent.
- WHEN an include allow-list is configured, a file is kept only if it matches an
  include pattern; the patterns use gitignore grammar so `*.md`, `cmd/`, and the
  like select content at any depth.
- WHILE deciding inclusion, directories are never filtered by the include list —
  the walk descends into every non-excluded directory so file-only include
  patterns remain reachable at any depth; only the exclude set prunes a subtree.
- WHEN evaluating exclusions, the matcher composes, in order, hard-coded
  DefaultPatterns, the repo-root `.gitignore`, the repo-root `.dexignore`, and
  `.dex/config.yml` `index.ignore`, and evaluates them with full gitignore
  semantics (anchoring, negation, `**`, dir-only patterns, later-pattern-wins).
- WHERE nested `.gitignore` files exist below the root, they are read only when
  `index.respect_nested_gitignore: true` is set (opt-in, default off, #74). When
  enabled the matcher walks the tree once (skipping `.git`/`node_modules`/
  `vendor`/`.dex`), re-anchors each nested pattern to its own directory following
  git rules — a pattern with a non-trailing `/` anchors to that directory
  (`/x`, `a/b` → `<dir>/x`, `<dir>/a/b`), an unanchored pattern matches at any
  depth below it (`x` → `<dir>/**/x`), trailing-slash dir markers and `!`
  negation are preserved — and appends them after the root files (shallower
  first, so a deeper file's rules win). Default off keeps the exclude set the
  predictable root-file + config model; nested `.dexignore` files are still not
  read.
- WHILE applying DefaultPatterns, the matcher always excludes vendored/build
  output trees, lockfiles and minified bundles, bundler output that keeps a
  `.js` extension (`*.bundle.js`, `*.chunk.js`), committed test-data dirs that
  follow the double-underscore tool convention (`__snapshots__/`, `__mocks__/`,
  `__fixtures__/` — the generic bare words `mocks/`/`fixtures/` are left to the
  project's own ignore files, per #457/#715), generated aggregates (`*.d.ts`,
  `llms.txt`), secret-shaped filenames (`.env`, `*.pem`, key files), and
  license-family files, independent of the project's own ignore files.
- WHEN a directory is matched, the matcher treats it as a directory (a trailing
  slash is applied) so gitignore dir-only patterns (`name/`) match correctly.
- WHEN a file passes include/exclude, the indexer keeps it only if its extension
  is in the indexable-extension allowlist OR its basename is a known indexable
  basename (e.g. `Makefile`, `Dockerfile`, `go.mod`), narrowing to chunkable
  source/prose.
- WHEN a file's first 8 KB contains a NUL byte, it is treated as binary and
  skipped, catching binaries that slipped through the extension allowlist.
- WHEN a file's first 4 KB matches a well-known secret pattern (AWS/GitHub/Slack/
  OpenAI/Stripe/GitLab/SendGrid keys, PEM blocks, …), it is skipped — UNLESS the
  path is a recognised test/fixture path, where fake credentials are expected
  input and the file is kept.

## Non-goals

- **Applying the rules.** Walking the tree, chunking, embedding, and the
  size/orchestration limits that consume these decisions are the **indexing**
  spec; here only the selection logic.
- **The on-disk index / config storage.** How the index is stored is **storage**;
  this spec only parses `.dex/config.yml`'s `index:` section for patterns.
- **Triggering re-selection on change.** Re-running selection when files change
  is the **watch** spec; watch reuses this matcher.
- **Per-language chunking detail.** Which extensions map to which tree-sitter
  grammar is the **indexing** spec; here only whether an extension is indexable.

## Checklist

- [x] Opt-in include allow-list (`.dex/config.yml` `index.include`); no include → select nothing
- [x] Include gates files only; directories descend (file-only patterns work at depth)
- [x] Exclude chain: DefaultPatterns + root `.gitignore` + root `.dexignore` + `index.ignore`, full gitignore semantics
- [x] Nested `.gitignore` files below root read only under `index.respect_nested_gitignore` (opt-in, default off); patterns re-anchored to their dir (#74)
- [x] DefaultPatterns cover vendor/build/lockfiles/minified/generated/secret-name/license families
- [x] Directory inputs get trailing-slash so dir-only patterns match
- [x] Indexable extension allowlist + known indexable basenames
- [x] Binary detection (NUL byte in first 8 KB)
- [x] Secret detection (first 4 KB regex panel), suppressed on test/fixture paths
- [x] Config parsed via `gopkg.in/yaml.v3` from `.dex/config.yml` `index:` section; no hand-rolled parser
- [x] Verified against the code by the verify workflow (flip to `living`)
