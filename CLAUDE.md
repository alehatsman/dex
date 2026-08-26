# dex

Local semantic-search MCP server for Claude Code (Go). Indexes a repo and serves two verbs — `query` (read) / `record` (write) — plus a `DEX_EXPERT` overlay of raw graph/search lanes over MCP.

## Workflow — issues (mgit or GitHub)

Issue/PR tracking runs on **one of two backends depending on the environment** — moongit (`mgit`) or GitHub (`gh`). Detect which is live (`mgit issue list` vs `gh issue list`) and use that one consistently for the session. No code without an owned issue.

| Step | mgit | GitHub (`gh`) |
|------|------|---------------|
| Survey | `mgit issue list --state todo,in_progress` | `gh issue list --state open` |
| Plan | `mgit issue create --title "<t>" --body "<plan>"` | `gh issue create --title "<t>" --body "<plan>"` |
| Claim | `mgit issue claim <n> --state in_progress` | assign yourself / label `in progress` (never take an already-claimed issue) |
| Progress | `mgit issue comment <n> --body "…"` | `gh issue comment <n> --body "…"` |
| Close | `mgit issue set-state <n> done` | `gh issue close <n>` |

With mgit, export your own `MOONGIT_TOKEN` (`mgt_…`); token name = your identity. One issue per unit of work.

Worktrees for non-trivial changes (outside repo at `~/worktrees/<repo>/<branch>`); never auto-push; ask before merge; conventional branches/commits. Merge FF-only: with mgit `mgit pr merge <n> --ff-only` (bare command defaults to a merge commit — always pass `--ff-only`); with GitHub, `gh pr merge --ff` is not a valid flag — true FF is `git push origin HEAD:main` after rebasing onto current main.

## Build / test — always via mooncake task

**Never use bare `go build`, `go test`, or `go install`** — requires `-tags sqlite_fts5` (FTS5/BM25); without it store tests panic and binary drops BM25.

```
mooncake task install   # build + install to ~/bin/dex
mooncake task test      # go test -tags sqlite_fts5 ./...
mooncake task ci-fast   # pre-commit gate (build + test + vet + fmt-check)
mooncake task ci        # full pre-push gate
```

`GO_TAGS=sqlite_fts5` is declared in `tasks.yml:37` and threaded automatically — do not repeat at call sites.

## Build-tag matrix

| Tag(s) | What it adds | Built by |
|--------|--------------|----------|
| `sqlite_fts5` (default) | FTS5 / BM25 search | `mooncake task install` / `ci` / `ci-fast` |
| `sqlite_fts5 onnx` | in-process ONNX embedder (`DEX_EMBED_ENGINE=onnx`) | `mooncake task ci-onnx` |

**ONNX** (`-tags onnx`): opt-in, links `internal/embed/onnx_engine.go`. Default build compiles a stub; onnx deps never enter the default build graph. Vectors live in a distinct namespace (`onnx:<model-id>:<dim>`); `EnsureEmbedModel` guard forces `dex reindex` if engine changes. Engine is static per index — ONNX and ollama/http vectors are never mixed.

ONNX runtime env (operator-provided; nothing bundled):
```
DEX_EMBED_ENGINE=onnx
DEX_ONNXRUNTIME_LIB=/path/to/libonnxruntime.so
DEX_ONNX_MODEL=/path/to/model.onnx
DEX_ONNX_TOKENIZER=/path/to/tokenizer.json
DEX_ONNX_DIM=384
DEX_ONNX_MODEL_ID=bge-small-en-v1.5
# optional: DEX_ONNX_MAX_SEQ DEX_ONNX_TOKEN_TYPES DEX_ONNX_INPUT_IDS DEX_ONNX_ATTENTION DEX_ONNX_TOKEN_TYPE DEX_ONNX_OUTPUT
```

See **[docs/deployment.md](docs/deployment.md)** for CPU-ONNX and BM25-only (`DEX_EMBED_ENGINE=none`) deployment modes.
