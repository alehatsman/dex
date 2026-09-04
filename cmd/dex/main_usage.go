package main

import (
	"fmt"
	"os"
)

// usageQuickstart prints a minimal (~10 line) getting-started screen.
// Shown on bare `dex` when stdin is a TTY.
func usageQuickstart() {
	fmt.Fprintln(os.Stderr, "dex — local semantic search for Claude Code\n\n"+
		"quickstart:\n"+
		"  dex setup                          guided first-run wizard (check + index + MCP)\n"+
		"  dex doctor                         verify endpoints, index dir, and MCP wiring\n"+
		"  dex index .                        build the per-project index (chunks + graph)\n"+
		`  dex ask "where is the watcher?"    one-shot router — picks intent, fuses search lanes`+"\n"+
		"  dex mcp                            run as MCP server (stdio) for Claude Code\n\n"+
		"  <path> defaults to cwd on every query/graph command.\n\n"+
		"run `dex help` for common commands · `dex help all` for the full reference")
}

// usageConcise prints ~30 lines covering the everyday command set.
// Shown by `dex help` / `dex --help` / `-h`.
func usageConcise() {
	fmt.Fprintln(os.Stderr, `dex — local semantic search for Claude Code

SEARCH & UNDERSTAND
  dex ask      [<path>] <q...>           START HERE — routed evidence pack (intent=assemble for a task working set)
  dex repo_map [--cluster <id>] [<path>] repo overview — run first in an unfamiliar repo
  dex search   [<path>] <q...>           hybrid search — raw ranking (ask composes this)
  dex read     <file>                    read a file (--mode signatures|skeleton|summary)
  dex locate   [<path>] <sym|path:line>  full context for one symbol: callers, tests, doc, blame
  dex trace    [<path>] <name>           call graph — --dir callers|callees|path|impact
  dex grep     [<path>] <pattern>        exact RE2 search (literals, import paths, no-embed fallback)

CHANGE SAFETY
  dex review_diff   [<path>]             per-hunk PR intelligence (--ref|--branch|--pr)
  dex check   [<path>] <ref...>          verify file:line[:symbol] references exist

STATUS
  dex status [<path>]                    endpoint health + index stats

MAINTENANCE
  dex index    <path>   build or refresh the index (--dry-run to preview)
  dex watch    <path>   keep the index fresh as files change
  dex reindex  <path>   drop and rebuild from scratch
  dex nuke     <path>   delete the on-disk index (destructive)

SETUP
  dex setup                          guided first-run wizard
  dex doctor                         check endpoints, config, MCP wiring
  dex env [--all] [--doc]            print effective DEX_* configuration
  dex mcp                            run as MCP server (stdio)
  dex serve [--addr] --project <p>   run as HTTP daemon (multi-project)
  dex hook <action>                  Claude Code hook scripts
  dex completion bash|zsh|fish       tab-completion script
  dex version                        print the build version

  run 'dex help all' for power lanes (refs, plan_rename, rehearse_patch, cohort, graph),
  build utilities (compact, compress, summarize, clone, bench), and full flag reference`)
}

// usageFull is the exhaustive reference — every subcommand, flag, and env var.
// Shown by `dex help all`.
func usageFull() {
	fmt.Fprintln(os.Stderr, `dex — local semantic search for Claude Code

quickstart:
  cd ~/code/myproject
  dex setup                          guided first-run wizard (check + index + MCP)
  dex index .                        build the per-project index (chunks + graph)
  dex ask "where is the watcher?"    one-shot router; emits suggested reads
  dex mcp                            run as MCP server (stdio) — point your agent at it
  dex env --doc                      see effective config with inline docs
  dex doctor                         check the setup is working end-to-end

  <path> defaults to cwd on every query/graph command.

query — read verbs (the everyday MCP agent surface is just 'query', which these
fold into; every "MCP: X" below is a granular tool that additionally requires
DEX_EXPERT=1 — it is NOT registered by default):
  dex repo_map [--cluster <id>] [<path>] repo orientation (MCP: repo_map,
                                          DEX_EXPERT). No --cluster: the
                                          first-touch bundle (L0 overview + a zoom into
                                          the most-central cluster). --cluster <id>: zoom
                                          a chosen cluster.
  dex ask    [<path>] <q...>            one-shot router (MCP: query — the only
                                          tool on the default surface). Picks intent,
                                          fuses semantic + symbol + graph; returns
                                          suggested_reads and a prose next_action.
                                          --intent assemble returns a task working set:
                                          ranked files, symbols, and the local rules
                                          (CLAUDE.md / specs) that govern them.
                                          Flags: --intent, --k, --format=text|json,
                                          --no-inline, --max-content-bytes, -v
  dex search [<path>] <q...>            hybrid semantic top-k chunks, fuses exact
                                          symbol-name hits via RRF (MCP: search, DEX_EXPERT).
                                          Raw ranking — ask composes this internally.
                                          Flags: --k, --rerank=off, --explain,
                                          --format=text|json, --max-content-bytes, -v
  dex read   <file>                     read a file (MCP: read, DEX_EXPERT). Modes:
                                          full (default; raw, no LLM), signatures,
                                          skeleton, map, aggressive, entropy, auto,
                                          analyze (per-mode token-cost comparison),
                                          and summary (LLM digest — needs a chat model).
                                          Flags: --mode, --start, --end, --focus,
                                          --temperature, --max-tokens, -v,
                                          --format=text|json
  dex locate [<path>] <target>          full context for one symbol: callers, tests,
                                          doc, blame (MCP: locate, DEX_EXPERT)
  dex trace  [<path>] <name>            call graph via --dir callers|callees|path|impact
                                          (MCP: trace, DEX_EXPERT)
  dex grep   [<path>] <pattern>         exact RE2 regex search (MCP: grep, DEX_EXPERT)
  dex review_diff   [<path>]            per-hunk PR intelligence (MCP: review_diff,
                                          DEX_EXPERT; query kind=review covers the
                                          everyday working-tree case on the default surface).
                                          Flags: --ref, --branch, --pr
  dex check  [<path>] <ref...>          verify file:line[:symbol] refs (MCP: check, DEX_EXPERT)
  dex status [<path>]                   endpoint health + project stats
                                          (MCP: status, DEX_EXPERT; alias: index status)
  dex index status [<path>]             same as dex status

query — power lanes (Go-focused or specialized; all DEX_EXPERT-only over MCP):
  dex refs   [<path>] <action> <sym>    type-precise Go symbol queries (MCP: refs,
                                          DEX_EXPERT). Actions: references, implementations,
                                          supertypes, subtypes.
  dex plan_rename    [<path>] <sym> <to> plan a type-precise rename — edit triples,
                                          never writes (MCP: plan_rename, DEX_EXPERT)
  dex rehearse_patch [<path>]            type-check a hypothetical edit in-memory,
                                          never writes (MCP: rehearse_patch, DEX_EXPERT).
                                          Flags: --edits, --file
  dex cohort [<path>] <iface>           Go interface lockstep set — types that must
                                          change together (MCP: cohort, DEX_EXPERT)

query — graph power lanes (CLI-only except deps; deps is DEX_EXPERT-only over MCP):
  dex graph neighbors [<path>] <file> <line>
                                          vector neighbours of a chunk (CLI-only)
  dex graph deps      [<path>] [flags]  package imports (MCP: deps, DEX_EXPERT)
                                          Flags: --file=<rel>, --package=<full path>
  dex graph links     [<path>] <doc>    markdown docs this doc links to (CLI-only)
  dex graph backlinks [<path>] <doc>    markdown docs that link to this doc (CLI-only)
                                          Flags: --k
  dex graph tags      [<path>] --tag=<t>|--doc=<d>
                                          tag→docs or doc→tags (CLI-only)
  dex graph export    [<path>]          dump graph_nodes/graph_edges as JSONL
                                          Flags: --output=<dir>

build / maintenance:
  dex index   <path>                    build or refresh the index. Runs chunk+embed
                                          AND the Go static graph. Flags: --graph=off
                                          skips graph, --graph=only refreshes just the
                                          graph layer. Other flags: -v, --force,
                                          --dry-run, --format=text|json
  dex watch   <path>                    keep the index fresh as files change
  dex reindex <path>                    drop and re-embed from scratch
  dex reindex --all --yes               drop and re-embed every known project
                                          (run `+"`dex index <path>`"+` once first to record it)
  dex clone   <src> <dst>               seed dst's index from src's (e.g. for a new
                                          worktree); follow with `+"`dex index <dst>`"+`
  dex nuke    <path>                    delete the on-disk index (prompts on TTY;
                                          pass --yes for scripts)
  dex summarize [<path>...]             generate per-file LLM summaries
                                          (isolated table; --get to read back)
  dex bench   <sub> [<path>]            benchmarks: eval|corpus|compress|perf|locomo
  dex feedback [--json]                 ask suggested_reads relevance on real traffic
                                          (reads hooks.jsonl)

content prep (LLM context utilities):
  dex compact <path>                    dump all indexable files to stdout with
                                          `+"`===== <relpath> =====`"+` headers.
                                          Flags: --out FILE, --max-bytes N, --strip
  dex compress <file|->                 run dex compression engine on a file or stdin —
                                          no LLM call. Flags: --mode=auto|aggressive|
                                          entropy|terse|json|off, --ext, --format=text|json

config / setup:
  dex env                               print effective env-var config with sources
                                          Flags: --all, --doc, -v, --format=text|json
  dex setup                             guided first-run wizard: check endpoints,
                                          offer to index cwd, write Claude Code
                                          routing rules. Flags: --check
  dex doctor                            check the setup: index dir, endpoints, config, MCP wiring
                                          Flags: -v
  dex config init                       scaffold .dex/config.yml with commented defaults
                                          Flags: --force, --full
  dex mcp                               run as an MCP server over stdio
  dex serve [flags] --project <p>       run as an HTTP daemon (multi-project).
                                          Flags: --addr=:8080 (default loopback
                                          when no token), --project (repeatable).
                                          DEX_SERVE_TOKEN gates non-loopback.
  dex hook inject                       Claude Code UserPromptSubmit hook:
                                          inject dex context before each turn.
  dex hook rewrite                      Claude Code PreToolUse(Bash) hook:
                                          rewrite rg/grep to dex search.
  dex hook redirect                     Claude Code PreToolUse(Read/Grep/…) hook:
                                          compress large files to save tokens.
  dex hook observe                      Claude Code PostToolUse/Stop hook:
                                          append event to hooks.jsonl log.
  dex completion bash|zsh|fish          output shell tab-completion script
  dex version                           print the build version

env:
  Run `+"`dex env`"+` for the effective configuration. The 5 vars that
  matter for 80% of setups: DEX_EMBED_URL, DEX_EMBED_MODEL,
  DEX_INDEX_DIR, DEX_CHAT_URL, DEX_CHAT_MODEL.
  Tuning knobs (timeouts, batch sizes, optional rerank/expand
  endpoints) — see docs/tuning.md or run `+"`dex env --all --doc`"+`.

exit codes:
  0    success
  1    error — runtime (index not found, embed unreachable) or usage
       (bad flags, missing arguments); check also exits 1 on failure
  2    unknown command
  130  interrupted (SIGINT / Ctrl-C)

  dex setup --check exits 1 when setup is incomplete.`)
}

// splitProjectArg peels an optional <path> off the front of a
// command's positional args. If args[0] resolves as an existing
// directory, use it; otherwise default to "." and pass every arg
// through to the caller. Matches git/rg ergonomics — `dex ask "where
// is X"` works from inside a project root without an explicit path.
//
// Trade-off: a typo'd path like `dex ask /tpyo "q"` will be treated
// as part of the question rather than triggering a clean "path does
// not exist" error. The cost of that ambiguity is small compared to
// requiring a path on every invocation.
