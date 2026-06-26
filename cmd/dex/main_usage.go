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

verbs (match the MCP tools — run "dex help all" for the full reference):
  dex map    [--cluster <id>] [<path>]   repo orientation: first-touch bundle, or --cluster to zoom
  dex find   [<path>] <q...>             semantic + symbol search
  dex lookup [<path>] <name>             exact identifier lookup
  dex read   <file>                      read a file — default raw (no LLM); --mode for views/summary
  dex trace  [<path>] <name>             call graph — --dir callers|callees|path
  dex impact [<path>] <name>             transitive blast-radius (callers, by depth)
  dex locate [<path>] <sym|path:line>    one-call orientation: callers, tests, doc, blame, notes
  dex review [<path>]                    per-hunk PR intelligence — --ref|--branch|--pr
  dex refactor [<path>] <symbol> <to>    plan a type-precise rename (edit triples; never writes)
  dex cohort [<path>] <interface>        types that must change in lockstep with an interface
  dex ask    [<path>] <q...>             one-shot router: semantic + symbol + graph
  dex grep   [<path>] <pattern>          exact RE2 regex search
  dex ls     [<path>]                    indexed file tree + chunk counts
  dex shell  <command...>                run a command with compressed output

detail / power lanes:
  dex graph  <sub> [<path>] ...          deps/callers/callees/links/path/diff/clusters
  dex notes  add|list|delete|gc          per-project notes (MCP: notes)
  dex status [<path>]                    endpoint health + project stats (alias: index status)

build / maintenance:
  dex index <path>                   build or refresh the index  (--dry-run to preview)
  dex watch <path>                   keep the index fresh as files change
  dex reindex <path>                 drop and re-embed from scratch
  dex summarize [<path>...]          generate per-file LLM summaries (isolated table; --get to read)
  dex nuke <path>                    delete the on-disk index
  dex bench <sub> [<path>]            benchmarks: eval|corpus|compress|perf|locomo
  dex proxy <path>                    MCP proxy — forward tools to a remote dex server

config / setup:
  dex setup                          guided first-run wizard
  dex config init                    scaffold .dex/config.yml with commented defaults
  dex env [--all] [--doc]            print effective DEX_* configuration
  dex doctor                         check setup: endpoints, index dir, MCP wiring
  dex mcp                            run as an MCP server over stdio
  dex completion bash|zsh|fish       shell tab-completion scripts
  dex version                        print the build version

  run 'dex help all' for the full reference (every subcommand, flag, env var, examples)`)
}

// usageFull is the exhaustive reference — the original usage() content plus exit codes.
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

query (the CLI verbs share the MCP tool names, #354/#427):
    dex find   [<path>] <q...>    semantic + symbol search (MCP: find)
    dex lookup [<path>] <name>    exact identifier lookup (MCP: lookup)
    dex trace  [<path>] <name>    call graph via --dir callers|callees|path
    dex impact [<path>] <name>    transitive caller blast-radius
    dex locate [<path>] <target>  one-call orientation (MCP: locate)
    dex review [<path>]           per-hunk PR intelligence (MCP: review)
    dex refactor [<path>] <s> <t> plan a type-precise rename (MCP: refactor)
    dex cohort [<path>] <iface>   interface lockstep set (MCP: cohort)
    (map / read / ask are already top-level — see below)

  dex ask [<path>] <q...>            one-shot router (MCP: ask). Picks intent,
                                          fuses semantic + symbol + graph; returns
                                          suggested_reads and a prose next_action.
                                          Flags: --intent, --k, --format=text|json,
                                          --no-inline, --max-content-bytes, -v
  dex find [<path>] <q...>           hybrid semantic top-k chunks (MCP: find)
                                          Flags: --k, --rerank=off, --explain,
                                          --format=text|json, --max-content-bytes, -v
  dex lookup [<path>] <name>         exact identifier lookup (MCP: lookup)
                                          Flags: --k, --format=text|json,
                                          --max-content-bytes, -v
  dex graph neighbors [<path>] <file> <line>
                                          vector neighbours of a chunk (CLI-only)
  dex graph deps [<path>] [flags]    package imports (MCP: deps)
                                          Flags: --file=<rel>, --package=<full path>
  dex graph callers [<path>] <name>  incoming calls edges (MCP: callers)
                                          Flags: --package=<pkg>, --k
  dex graph callees [<path>] <name>  outgoing calls edges (MCP: callees)
                                          Flags: --package=<pkg>, --k
  dex graph links [<path>] <doc>     markdown docs this doc links to (CLI-only)
                                          Flags: --k
  dex graph backlinks [<path>] <doc> markdown docs that link to this doc (CLI-only)
                                          Flags: --k
  dex graph tags [<path>] --tag=<t>|--doc=<d>
                                          tag→docs or doc→tags (CLI-only)
                                          Flags: --k
  dex graph export [<path>]          dump graph_nodes/graph_edges as JSONL
                                          Flags: --output=<dir>
  dex map [--cluster <id>] [<path>]  repo orientation (MCP: map). No --cluster: the
                                          first-touch bundle (L0 overview + a zoom into
                                          the most-central cluster). --cluster <id>: zoom
                                          a chosen cluster.
  dex read <file>                    read a file (MCP: read). Modes:
                                          full (default; raw, no LLM), signatures,
                                          aggressive, entropy, auto, and summary
                                          (LLM digest — needs a chat model).
                                          Flags: --mode, --start, --end, --focus,
                                          --temperature, --max-tokens, -v,
                                          --format=text|json
  dex status [<path>]                endpoint health + project stats
                                          (MCP: status; alias for index status)
  dex index status [<path>]          same as dex status

build / maintenance:
  dex index <path>                   build or refresh the index. Runs chunk+embed
                                          AND the Go static graph. Flags: --graph=off
                                          skips graph, --graph=only refreshes just the
                                          graph layer. Other flags: -v, --force,
                                          --dry-run, --format=text|json
  dex env                            print effective env-var config with sources
                                          Flags: --all, --doc, -v, --format=text|json
  dex compact <path>                 concatenate indexable files under <path>
                                          to stdout with `+"`===== <relpath> =====`"+`
                                          headers. Honors .gitignore/.dexignore
                                          and skips binaries + secret-shaped files.
                                          Flags: --out FILE, --max-bytes N, --strip
  dex compress <file|->              compress a file or stdin through the dex
                                          engine — no LLM call. Writes to stdout
                                          or --out. Flags: --mode=auto|aggressive|
                                          entropy|terse|json|off, --ext, --format=text|json
  dex notes add|list|delete|gc       CLI access to the per-project knowledge
                                          store (MCP: notes). add stores
                                          a fact (--archetype, --confidence),
                                          list shows top-k by salience (--k),
                                          delete removes by id, gc runs decay +
                                          consolidate + evict (--max-facts).
                                          query/rm are accepted aliases.
                                          Flags: --format=text|json
  dex nuke   <path>                  delete the on-disk index for a project
                                          (prompts on TTY; pass --yes for scripts)
  dex reindex <path>                 drop and re-embed from scratch
  dex reindex --all --yes            drop and re-embed every known project
                                          (skips indexes from before this feature;
                                          run `+"`dex index <path>`"+` once to
                                          re-record them)
  dex watch  <path>                  keep the index fresh as files change
  dex clone  <src> <dst>             seed dst's index from src's (e.g. for a
                                          new git worktree); follow with
                                          `+"`dex index <dst>`"+` to reconcile
  dex bench  <sub> [<path>]          benchmarks: eval|corpus|compress|perf|locomo
  dex mcp                            run as an MCP server over stdio
  dex serve [flags] --project <p>    run as an HTTP daemon (multi-project).
                                          Flags: --addr=:8080 (default loopback
                                          when no token), --project (repeatable).
                                          DEX_SERVE_TOKEN gates non-loopback.
  dex proxy <path>                   MCP proxy — forward tools to a remote dex server
  dex hook inject                    Claude Code UserPromptSubmit hook:
                                          inject dex context before each turn.
  dex hook rewrite                   Claude Code PreToolUse(Bash) hook:
                                          rewrite rg/grep to dex find.
  dex hook redirect                  Claude Code PreToolUse(Read/Grep/…) hook:
                                          compress large files to save tokens.
  dex hook observe                   Claude Code PostToolUse/Stop hook:
                                          append event to hooks.jsonl log.
  dex setup                          guided first-run wizard: check endpoints,
                                          offer to index cwd, write Claude Code
                                          routing rules. Flags: --check
  dex doctor                         check the setup: index dir, endpoints, config, MCP wiring
                                          Flags: -v
  dex config init                    scaffold .dex/config.yml with commented defaults
                                          Flags: --force, --full
  dex completion bash|zsh|fish       output shell tab-completion script
  dex version                        print the build version

env:
  Run `+"`dex env`"+` for the effective configuration. The 5 vars that
  matter for 80% of setups: DEX_EMBED_URL, DEX_EMBED_MODEL,
  DEX_INDEX_DIR, DEX_CHAT_URL, DEX_CHAT_MODEL.
  Tuning knobs (timeouts, batch sizes, optional rerank/expand
  endpoints) — see docs/tuning.md or run `+"`dex env --all --doc`"+`.

exit codes:
  0    success
  1    runtime error (index not found, embed unreachable, etc.)
  2    usage error (bad flags, missing arguments, unknown command)
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
