# dex

Local semantic-search MCP server for Claude Code (Go). Indexes a repo and
serves `search_semantic` / `ask` / `search_symbol` / `graph_*` over MCP.

## Workflow — track work as moongit issues (mgit)

Prereq: the repo has a `moongit` remote (code mirror) — or `MOONGIT_SERVER`
points at the server. Export your **own** `MOONGIT_TOKEN` (`mgt_…`); the
token's name is your identity in every claim/comment, so never share one.

1. **Survey:** `mgit issue list --state todo,in_progress`.
2. **Plan as issues** — one issue per unit of work, the plan in the body. Split
   multi-part work into multiple issues:
   `mgit issue create --title "<t>" --body "<plan>"`.
3. **Claim before coding:** `mgit issue claim <n> --state in_progress`. Never
   work an issue already `in_progress` under another identity.
4. **Report progress** at real checkpoints: `mgit issue comment <n> --body "…"`.
5. **Close out** when merged + verified: `mgit issue set-state <n> done`
   (`mgit issue unclaim <n>` if you drop it).

No code without an owned issue. Worktrees for non-trivial changes; never
auto-push; ask before merge; conventional branches/commits. Merge
fast-forward only: `mgit pr merge <n> --ff-only` — the bare command
defaults to a merge commit (moongit #360), so always pass `--ff-only`;
if `main` advanced, rebase your branch and re-merge.

## Build / test — always via mooncake task

**Never use bare `go build`, `go test`, or `go install` directly** — the
project requires `-tags sqlite_fts5` for FTS5 support (mattn/go-sqlite3)
and without it store tests panic and the built binary drops BM25 search.
The canonical commands:

```
mooncake task install   # build + install to ~/bin/dex
mooncake task test      # go test -tags sqlite_fts5 ./...
mooncake task ci-fast   # pre-commit gate (build + test + vet + fmt-check)
mooncake task ci        # full pre-push gate
```

`GO_TAGS=sqlite_fts5` is declared once in `tasks.yml:37` and threaded into
every task automatically. Do not repeat it at call sites.

## Build-tag matrix

> Running dex with no GPU? See **[docs/lean-profile.md](docs/lean-profile.md)** —
> the CPU-ONNX (`-tags onnx`) and BM25-only (`DEX_EMBED_ENGINE=none`) deployment
> modes, and the capability-derived tool surface they expose.

| Tag(s) | What it adds | Native dep | Built by |
|--------|--------------|------------|----------|
| `sqlite_fts5` (default) | FTS5 / BM25 search | cgo + libsqlite3 (already required) | `mooncake task install` / `ci` / `ci-fast` |
| `sqlite_fts5 onnx` | in-process ONNX embedder (`DEX_EMBED_ENGINE=onnx`) | onnxruntime `.so` at **runtime** only (dlopen'd) | `mooncake task ci-onnx` (compile guard) |

The **`onnx`** tag is opt-in and adds an in-process embedder so a binary can
embed locally with no embedding server (issue #180). It is gated exactly like
`sqlite_fts5`:

- The default build compiles a stub (`internal/embed/onnx_stub.go`) that
  returns *"onnx engine not built in: rebuild with -tags onnx"*. The default
  binary is **byte-identical** with or without these files present, and the
  onnx deps (`yalue/onnxruntime_go`, `sugarme/tokenizer`) **never enter the
  default build graph** — verify with `go list -deps ./...` (they're absent)
  vs `go list -tags onnx -deps ./internal/embed/` (present). So `ci` /
  `ci-fast` stay dep-free and unchanged.
- `-tags onnx` links the real engine (`internal/embed/onnx_engine.go`):
  tokenize (HF `tokenizer.json`) → onnxruntime session → attention-masked
  mean-pool → L2-normalize.

Build + run the onnx engine:

```
GO_TAGS="sqlite_fts5 onnx" mooncake task install      # build with onnx linked in
mooncake task ci-onnx                                  # compile+vet guard for the onnx lane

# runtime config (operator-provided; nothing is bundled or auto-downloaded):
export DEX_EMBED_ENGINE=onnx
export DEX_ONNXRUNTIME_LIB=/path/to/libonnxruntime.so  # dlopen'd at startup
export DEX_ONNX_MODEL=/path/to/model.onnx
export DEX_ONNX_TOKENIZER=/path/to/tokenizer.json
export DEX_ONNX_DIM=384                                # model's embedding dim
export DEX_ONNX_MODEL_ID=bge-small-en-v1.5             # identity baked into the index namespace
# optional: DEX_ONNX_MAX_SEQ (512), DEX_ONNX_TOKEN_TYPES (true),
#           DEX_ONNX_INPUT_IDS / DEX_ONNX_ATTENTION / DEX_ONNX_TOKEN_TYPE / DEX_ONNX_OUTPUT
```

ONNX vectors live in a **distinct namespace** — `ModelName()` is
`onnx:<model-id>:<dim>`, recorded in the index `meta` table. The store's
`EnsureEmbedModel` guard trips if an index built with one engine is queried
with another, forcing a `dex reindex`. The engine is **static per index**
(no hot-swap fallback): ONNX and ollama/http vectors are never mixed.

Inference correctness is covered by `internal/embed/onnx_engine_test.go` (a
golden test that compares ONNX output to the HTTP backend serving the same
model within a cosine tolerance). It is skip-gated on `DEX_ONNX_MODEL` +
`DEX_ONNX_TOKENIZER` + `DEX_ONNXRUNTIME_LIB` (+ `DEX_ONNX_GOLDEN_URL` for the
cross-check), so it only runs where real artifacts exist.
