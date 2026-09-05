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
		`  dex query "where is the watcher?"  the read verb — picks the lane, fuses search lanes`+"\n"+
		"  dex mcp                            run as MCP server (stdio) for Claude Code\n\n"+
		"  <path> defaults to cwd on every query/graph command.\n\n"+
		"run `dex help` for common commands · `dex help all` for the full reference")
}

// usageConcise prints ~30 lines covering the everyday command set.
// Shown by `dex help` / `dex --help` / `-h`.
func usageConcise() {
	fmt.Fprintln(os.Stderr, `dex — local semantic search for Claude Code
(MCP agents: 'query' is the only tool on by default — every DEX_EXPERT power
lane below (search/trace/grep/read/…) is a granular tool 'query' already
covers; all of them work from the CLI too, under DEX_EXPERT=1.)

SEARCH & UNDERSTAND
  dex query [--kind=K] [--want=W] [<path>] <input...>
                                          the read verb — START HERE. Input shape picks the
                                          lane: a path → signatures, path:line → a slice,
                                          /regex/ → grep, a bare symbol → its call graph,
                                          prose → a ranked evidence pack. --kind=assemble
                                          for a task working set, --kind=orient for a repo
                                          overview, --kind=review for the working-tree diff.

CHANGE SAFETY
  dex query --kind=check [<path>] <ref...>   verify file:line[:symbol] references exist

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
  dex index status [<path>]          endpoint health + index stats

  run 'dex help all' for power lanes (--kind=refs|cohort|deps, plan_rename, rehearse_patch, graph),
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
  dex query "where is the watcher?"  the read verb; emits suggested reads
  dex mcp                            run as MCP server (stdio) — point your agent at it
  dex env --doc                      see effective config with inline docs
  dex doctor                         check the setup is working end-to-end

  <path> defaults to cwd on every query/graph command.

query — the single read verb (#849, specs/query-unification.md). Its input
SHAPE picks the lane; --kind forces it. This is the everyday MCP/REST/CLI
surface alike — every transport parses its own format in, calls the same
dispatcher, and formats its own format out:
  dex query [flags] [<path>] <input...>  read the codebase intelligence.
                                          A path → compressed signatures, a
                                          path:line/range → that slice, a
                                          /regex/ → grep, a bare symbol → its
                                          call graph, prose → a ranked
                                          semantic evidence pack.
                                          Flags: --kind, --want, --to,
                                          --budget, --project-root, --k,
                                          --context, --fixed, --format=text|json
  dex query --kind=search    <q...>      hybrid semantic top-k chunks
  dex query --kind=assemble  <task...>   budget-bounded task working set:
                                          ranked files, symbols, governing rules
  dex query --kind=architecture|packages|orient  repo-orientation packs
  dex query --kind=review                per-hunk intelligence for the
                                          working-tree diff (HEAD~1..HEAD) —
                                          for a targeted PR/branch/ref, use the
                                          review_diff MCP tool (DEX_EXPERT)
  dex query --kind=read      <file>      compressed signatures (default) —
                                          --want=full for raw content,
                                          --want=map|skeleton|lines:N-M for
                                          other facets. Raw full/aggressive/
                                          entropy/auto/summary modes, --ref
                                          (historical read), and --handle
                                          (session expansion) are MCP/DEX_EXPERT
                                          read-tool-only now, no CLI front door.
  dex query --kind=grep      <pattern>   exact RE2 regex search.
                                          Flags: --context, --fixed
  dex query --kind=locate    <sym|path:line>  full context for one symbol:
                                          callers, tests, doc, blame
  dex query --kind=callers|callees|impact|path <sym>  call-graph traversal
                                          (--to sets the path facet's destination)
  dex query --kind=check     <ref...>    verify file:line[:symbol] refs exist
  dex query --kind=refs --want=<action> <sym>  type-precise Go symbol queries
                                          (references|implementations|
                                          supertypes|subtypes)
  dex query --kind=cohort    <iface>     Go interface lockstep set — types
                                          that must change together
  dex query --kind=deps      <path|pkg>  package import edges. A relative
                                          package DIRECTORY (not a file) as
                                          the sole positional is ambiguous
                                          with the leading <path> project
                                          arg — pass --project-root, a file
                                          inside the package, or the full
                                          import path instead.
  dex query --kind=status                endpoint health (cross-project; for
                                          single-project index stats use
                                          `+"`dex index status`"+`)

  Dropped in the collapse (no longer a CLI front door — the underlying
  MCP tools still carry them under DEX_EXPERT): search's --explain score
  breakdown; grep's --ext/--in/--max-results/structural --query/--lang;
  trace's --package/--max-depth; review_diff's --ref/--branch/--pr/--worktree/
  --compact selectors; repo_map's --cluster/--around/--around-diff zooms.

  dex graph neighbors [<path>] <file> <line>
                                          vector neighbours of a chunk (CLI-only)
  dex graph links     [<path>] <doc>    markdown docs this doc links to (CLI-only)
  dex graph backlinks [<path>] <doc>    markdown docs that link to this doc (CLI-only)
                                          Flags: --k
  dex graph tags      [<path>] --tag=<t>|--doc=<d>
                                          tag→docs or doc→tags (CLI-only)
  dex graph export    [<path>]          dump graph_nodes/graph_edges as JSONL
                                          Flags: --output=<dir>
  dex plan_rename    [<path>] <sym> <to> plan a type-precise rename — edit triples,
                                          never writes (MCP: plan_rename, DEX_EXPERT;
                                          different contract from query — returns an
                                          edit plan, not a read)
  dex rehearse_patch [<path>]            type-check a hypothetical edit in-memory,
                                          never writes (MCP: rehearse_patch, DEX_EXPERT).
                                          Flags: --edits, --file

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
// through to the caller. Matches git/rg ergonomics — `dex query "where
// is X"` works from inside a project root without an explicit path.
//
// Trade-off: a typo'd path like `dex query /tpyo "q"` will be treated
// as part of the input rather than triggering a clean "path does
// not exist" error. The cost of that ambiguity is small compared to
// requiring a path on every invocation.
