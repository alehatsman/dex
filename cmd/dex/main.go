// dex — local semantic-search helper for Claude Code.
//
// The query-side subcommands mirror the MCP tool surface 1:1
// (subcommand-group form). The build/maintenance commands are CLI-only.
//
//	ask <path> <q...>             Primary entry point (MCP: ask).
//	search semantic <path> <q...> Hybrid top-k chunks (MCP: find).
//	search symbol <path> <name>   Exact identifier lookup (MCP: lookup).
//	graph neighbors <path> <file> <line>
//	                              Vector neighbours of a chunk (CLI-only).
//	graph deps <path> [--file|--package]
//	                              `imports` edges for a file/package (MCP: deps).
//	graph callers <path> <name>   Incoming `calls` edges (MCP: callers).
//	graph callees <path> <name>   Outgoing `calls` edges (MCP: callees).
//	graph links <path> <doc>      Docs this markdown doc links to (CLI-only).
//	graph backlinks <path> <doc>  Docs that link to this markdown doc (CLI-only).
//	graph tags <path> --tag|--doc Tag→docs or doc→tags (CLI-only).
//	graph export <path>           Dump nodes/edges as JSONL (CLI-only).
//	view summarize <path> <file>  Summarize a file slice (MCP: read).
//	read <file>                   Structural read (signatures/aggressive/entropy); no LLM.
//	index <path>                  Build or refresh the per-project index.
//	index status [<path>]         Endpoint health + indexed projects (MCP: status).
//	generate <path> <prompt>      Generate code grounded in the project's index.
//	env                           Print effective env-var configuration.
//	compact <path>                Concatenate indexable files for LLM prompts (alias: bundle).
//	nuke <path>                   Delete the on-disk index for a project.
//	reindex <path>|--all          Drop and re-embed.
//	watch <path>                  Keep the index fresh as files change.
//	clone <src> <dst>             Seed dst's index from src's (worktrees).
//	hook inject                   Claude Code UserPromptSubmit hook — injects dex context.
//	hook rewrite                  Claude Code PreToolUse(Bash) hook — rewrites rg/grep to dex.
//	hook redirect                 Claude Code PreToolUse(Read/Grep/…) hook — compresses large files.
//	hook observe                  Claude Code PostToolUse/Stop hook — appends event log.
//	bench locomo <path>           LoCoMo memory-recall benchmark (recall@k / token-F1).
//	knowledge add|query|rm|gc     Store/list/delete/gc per-project facts (MCP: notes).
//	compress <file|->             Compress a file or stdin through the dex engine (no LLM).
//	compress-stdin                Compress stdin through dex patterns; writes to stdout.
//	shell-hook                    Print eval-able shell hook for passive output compression.
//	setup                         Guided first-run wizard: check endpoints, index cwd, write Claude routing rules.
//	mcp                           Run as an MCP server over stdio. Tool surface is capability-derived.
//	version                       Print the build version.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/alehatsman/dex/internal/chat"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/alehatsman/dex/internal/chunk"
	"github.com/alehatsman/dex/internal/embed"
	"github.com/alehatsman/dex/internal/graph"
	"github.com/alehatsman/dex/internal/ignore"
	"github.com/alehatsman/dex/internal/index"
	"github.com/alehatsman/dex/internal/lock"
	"github.com/alehatsman/dex/internal/mcp"
	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/rerank"
	"github.com/alehatsman/dex/internal/store"
	"github.com/alehatsman/dex/internal/watch"
)

func main() {
	if len(os.Args) < 2 {
		// TTY: friendly quickstart. Non-TTY: short error.
		if stdinIsTTY() {
			usageQuickstart()
		} else {
			fmt.Fprintln(os.Stderr, "dex: no command given — run 'dex help'")
		}
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]

	// Fill DEX_* gaps from .dex/config.yml in the working dir (env still wins).
	// Collapses the env-var sprawl into one per-project file; see config_file.go.
	if wd, werr := os.Getwd(); werr == nil {
		if cerr := applyProjectConfig(wd); cerr != nil {
			fmt.Fprintf(os.Stderr, "dex: %v\n", cerr)
			os.Exit(2)
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	var err error
	switch cmd {
	case "index", "idx":
		err = cmdIndexDispatch(ctx, args)
	case "ask":
		err = cmdAsk(ctx, args)
	case "search":
		err = cmdSearch(ctx, args)
	case "view":
		err = cmdView(ctx, args)
	case "read":
		err = cmdRead(ctx, args)
	case "graph":
		err = cmdGraph(ctx, args)
	case "map":
		err = cmdMap(ctx, args)
	case "orient":
		err = cmdOrient(ctx, args)
	// Verb facade front doors (#354) — mirror the default MCP tool surface.
	// map/read/ask already exist above; these thin-wrap search/graph.
	case "find":
		err = cmdFind(ctx, args)
	case "trace":
		err = cmdTrace(ctx, args)
	case "impact":
		err = cmdImpact(ctx, args)
	case "generate":
		err = cmdGenerate(ctx, args)
	case "env":
		err = cmdEnv(ctx, args)
	case "compact", "bundle":
		err = cmdCompact(ctx, args)
	case "nuke":
		err = cmdNuke(ctx, args)
	case "reindex":
		err = cmdReindex(ctx, args)
	case "mcp":
		err = cmdMCP(ctx, args)
	case "serve":
		err = cmdServe(ctx, args)
	case "proxy":
		err = cmdProxy(ctx, args)
	case "watch":
		err = cmdWatch(ctx, args)
	case "clone":
		err = cmdClone(ctx, args)
	case "hook":
		err = cmdHook(ctx, args)
	case "knowledge":
		err = cmdKnowledge(ctx, args)
	case "compress":
		err = cmdCompress(args)
	case "compress-stdin":
		err = cmdCompressStdin(args)
	case "shell-hook":
		err = cmdShellHook(args)
	case "setup":
		err = cmdSetup(ctx, args)
	case "doctor":
		err = cmdDoctor(ctx, args)
	case "completion":
		err = cmdCompletion(args)
	case "bench":
		err = runBench(ctx, args)
	case "config":
		err = cmdConfig(args)
	case "version", "-V", "--version":
		fmt.Println(mcp.Version)
		return
	case "-h", "--help":
		usageConcise()
		return
	case "help":
		if len(args) > 0 && args[0] == "all" {
			usageFull()
		} else {
			usageConcise()
		}
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		usageConcise()
		os.Exit(2)
	}
	if err != nil {
		// `-h` returns flag.ErrHelp via flag.ContinueOnError. The FlagSet
		// already printed its usage block; suppress the redundant
		// "flag: help requested" line and exit cleanly.
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		// SIGINT/SIGTERM cancel ctx; report a friendlier exit (130 is
		// the conventional shell code for SIGINT).
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "interrupted")
			os.Exit(130)
		}
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

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
		"  <path> defaults to cwd on every query/view/graph command.\n\n"+
		"run `dex help` for common commands · `dex help all` for the full reference")
}

// usageConcise prints ~30 lines covering the everyday command set.
// Shown by `dex help` / `dex --help` / `-h`.
func usageConcise() {
	fmt.Fprintln(os.Stderr, `dex — local semantic search for Claude Code

verbs (match the MCP tools — run "dex help all" for the full reference):
  dex map    [--cluster <id>] [<path>]   deterministic repo orientation map (L0/L1)
  dex find   [<path>] <q...>             semantic + symbol search
  dex read   <file>                      structural file read (signatures; no LLM)
  dex trace  [<path>] <name>             call graph — --dir callers|callees|path
  dex impact [<path>] <name>             transitive blast-radius (callers, by depth)
  dex ask    [<path>] <q...>             one-shot router: semantic + symbol + graph

detail / power lanes:
  dex search semantic|symbol [<path>]    raw hybrid / exact-symbol search
  dex graph  <sub> [<path>] ...          deps/callers/callees/links/path/diff/communities
  dex view   summarize [<path>] <file>   LLM file summary
  dex knowledge add|query|rm|gc          per-project notes
  dex index  status [<path>]             endpoint health + project stats

build / maintenance:
  dex index <path>                   build or refresh the index  (--dry-run to preview)
  dex watch <path>                   keep the index fresh as files change
  dex reindex <path>                 drop and re-embed from scratch
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

  <path> defaults to cwd on every query/view/graph command.

query (mirrors the MCP tool surface):
  verb front doors (thin aliases over the lanes below, #354):
    dex find   [<path>] <q...>    semantic + symbol search  (= search semantic)
    dex trace  [<path>] <name>    call graph via --dir callers|callees|path
    dex impact [<path>] <name>    transitive caller blast-radius
    (map / read / ask are already top-level — see below)

  dex ask [<path>] <q...>            one-shot router (MCP: ask). Picks intent,
                                          fuses semantic + symbol + graph; returns
                                          suggested_reads and a prose next_action.
                                          Flags: --intent, --k, --format=text|json,
                                          --no-inline, --max-content-bytes, -v
  dex search semantic [<path>] <q...> hybrid top-k chunks (MCP: find)
                                          Flags: --k, --rerank=off, --explain,
                                          --format=text|json, --max-content-bytes, -v
  dex search symbol [<path>] <name>  exact identifier lookup (MCP: lookup)
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
  dex view summarize [<path>] <file> summarize a file slice via the chat model
                                          (MCP: read). Flags: --start, --end,
                                          --focus, --temperature, --max-tokens, -v,
                                          --format=text|json
  dex read <file>                    structural read — no LLM call. Modes:
                                          auto|full|signatures|aggressive|entropy.
                                          Flags: --mode, --start, --end,
                                          --format=text|json
  dex index status [<path>]          endpoint health + project stats
                                          (MCP: status)

build / maintenance:
  dex index <path>                   build or refresh the index. Runs chunk+embed
                                          AND the Go static graph. Flags: --graph=off
                                          skips graph, --graph=only refreshes just the
                                          graph layer. Other flags: -v, --force,
                                          --dry-run, --format=text|json  dex generate <path> <prompt>       RAG: top-k chunks → chat endpoint
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
                                          entropy|terse|off, --ext, --format=text|json
  dex knowledge add|query|rm|gc      CLI access to the per-project knowledge
                                          store (MCP: notes). add stores
                                          a fact (--archetype, --confidence),
                                          query lists top-k by salience (--k),
                                          rm deletes by id, gc runs decay +
                                          consolidate + evict (--max-facts).
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
  dex mcp                            run as an MCP server over stdio
  dex serve [flags] --project <p>    run as an HTTP daemon (multi-project).
                                          Flags: --addr=:8080 (default loopback
                                          when no token), --project (repeatable).
                                          DEX_SERVE_TOKEN gates non-loopback.
  dex hook inject                    Claude Code UserPromptSubmit hook:
                                          inject dex context before each turn.
  dex hook rewrite                   Claude Code PreToolUse(Bash) hook:
                                          rewrite rg/grep to dex search.
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
func splitProjectArg(args []string) (path string, rest []string) {
	if len(args) > 0 {
		if st, err := os.Stat(args[0]); err == nil && st.IsDir() {
			return args[0], args[1:]
		}
	}
	return ".", args
}

// validIntent reports whether s is one of the strategies the context
// router accepts. Empty string means "auto" and is allowed.
func validIntent(s string) bool {
	switch s {
	case "", "auto", "behavior_search", "symbol_lookup", "callers", "callees",
		"architecture", "package_topology", "editing_context":
		return true
	}
	return false
}

// boolFlag duck-types the stdlib's unexported flag.boolFlag interface so
// reorderFlags can tell standalone boolean flags (`-v`) from flags that
// consume a value as the next token (`--rerank off`).
type boolFlag interface {
	flag.Value
	IsBoolFlag() bool
}

// reorderFlags moves every flag-shaped token to the front of args so
// flag.Parse sees them even when the user typed them after positional
// args. Without this, Go's flag package silently stops parsing at the
// first non-flag arg and quietly drops every flag that follows — a
// real footgun for invocations like `dex search semantic <path> "q" --k=3`.
//
// Uses the FlagSet to detect which flags consume a separate-token value
// (so `--rerank off` is treated as one flag/value pair, not flag plus
// stray positional). `--` ends flag scanning, matching stdlib behavior.
func reorderFlags(fs *flag.FlagSet, args []string) []string {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positional = append(positional, args[i:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			positional = append(positional, a)
			continue
		}
		flags = append(flags, a)
		if strings.Contains(a, "=") {
			continue
		}
		name := strings.TrimLeft(a, "-")
		f := fs.Lookup(name)
		if f == nil {
			// Unknown flag — let fs.Parse raise the error.
			continue
		}
		if bf, ok := f.Value.(boolFlag); ok && bf.IsBoolFlag() {
			continue
		}
		if i+1 < len(args) {
			flags = append(flags, args[i+1])
			i++
		}
	}
	return append(flags, positional...)
}

// setHelp wires `<cmd> -h` to print a one-line summary, a usage pattern
// showing positional args, the auto-generated flag defaults, optional
// examples, and a pointer to the full reference.
// The variadic examples parameter accepts concrete invocation lines; each
// is printed under an "examples:" header. Pass none to omit that section.
func setHelp(fs *flag.FlagSet, oneLiner, usagePattern string, examples ...string) {
	fs.Usage = func() {
		out := fs.Output()
		fmt.Fprintln(out, oneLiner)
		fmt.Fprintln(out)
		fmt.Fprintln(out, "usage:")
		fmt.Fprintln(out, "  "+usagePattern)
		hasFlags := false
		fs.VisitAll(func(*flag.Flag) { hasFlags = true })
		if hasFlags {
			fmt.Fprintln(out)
			fmt.Fprintln(out, "flags:")
			fs.PrintDefaults()
		}
		if len(examples) > 0 {
			fmt.Fprintln(out)
			fmt.Fprintln(out, "examples:")
			for _, ex := range examples {
				fmt.Fprintln(out, "  "+ex)
			}
		}
		fmt.Fprintln(out)
		fmt.Fprintln(out, "see also: dex help all")
	}
}

// ─── env helpers ──────────────────────────────────────────────────────────

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func indexDir() (string, error) {
	if v := os.Getenv("DEX_INDEX_DIR"); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cache", "dex"), nil
}

// storeOpts reads runtime tweaks from the environment so every code
// path that opens a Store sees the same configuration.
func storeOpts() store.Options {
	opts := store.Options{
		SearchOptions: store.SearchOptions{
			DisableBM25:    os.Getenv("DEX_DISABLE_BM25") == "1",
			MaxHitsPerFile: maxHitsPerFile(),
			FusionMode:     fusionMode(),
			FusionAlpha:    fusionAlpha(),
		},
		GraphOptions: store.GraphOptions{
			GraphGamma:      graphGamma(),
			GraphHopCap:     graphHopCap(),
			GraphLaneWeight: graphLaneWeight(),
		},
		RerankOptions: store.RerankOptions{
			RerankPool: rerankPool(),
		},
		InfraOptions: store.InfraOptions{
			DisableCoAccess: os.Getenv("DEX_COACCESS") == "0",
			VectorQuant:     vectorQuant(),
		},
	}
	// Assign through a typed-nil check: a (*rerank.Client)(nil) stored
	// in the Reranker interface field would still compare != nil, and
	// store.Search would dispatch into a nil receiver.
	if rc := newRerankClient(); rc != nil {
		opts.Reranker = rc
	}
	return opts
}

// vectorQuant reads DEX_VECTOR_QUANT — the chunk_vecs KNN encoding.
// "int8" selects scalar quantization (~4× smaller, faster integer cosine);
// anything else (incl. unset) keeps full-precision float32. Flipping it on
// an existing index rebuilds chunk_vecs from chunks.vec on the next Open.
func vectorQuant() string {
	raw := strings.TrimSpace(os.Getenv("DEX_VECTOR_QUANT"))
	switch strings.ToLower(raw) {
	case "", "none", "float32", "f32", "int8":
		return raw
	default:
		fmt.Fprintf(os.Stderr, "warning: DEX_VECTOR_QUANT=%q unrecognized; using float32\n", raw)
		return ""
	}
}

// fusionMode reads DEX_FUSION_MODE — score-fusion strategy for the dense+BM25 lanes.
// Default is FusionLinear (convex combination, α=fusionAlpha). Calibrated in #317:
// a leave-one-repo-out sweep over the multi-repo corpus picked FusionLinear α=0.7 in
// all 5 folds (+3.3% NDCG / +3pts Recall vs RRF, de-contaminated), and a dex-self
// confirmation showed +145% NDCG / +113% Recall over RRF. Set DEX_FUSION_MODE=rrf to
// fall back to rank-only Reciprocal Rank Fusion.
func fusionMode() store.FusionMode {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DEX_FUSION_MODE"))) {
	case "", "linear":
		return store.FusionLinear
	case "rrf":
		return store.FusionRRF
	default:
		fmt.Fprintf(os.Stderr, "warning: DEX_FUSION_MODE=%q unrecognized; using linear\n", os.Getenv("DEX_FUSION_MODE"))
		return store.FusionLinear
	}
}

// fusionAlpha reads DEX_FUSION_ALPHA — dense weight for FusionLinear (0 < α ≤ 1).
// Default 0.7: the leave-one-repo-out corpus sweep in #317 found a clean interior
// optimum at α=0.7 (NDCG peaked there and fell off toward both pure-BM25 and
// pure-dense), confirmed on dex-self. Values outside (0,1] are rejected with a warning.
func fusionAlpha() float32 {
	raw := os.Getenv("DEX_FUSION_ALPHA")
	if raw == "" {
		return 0.7
	}
	v, err := strconv.ParseFloat(raw, 32)
	if err != nil || v <= 0 || v > 1 {
		fmt.Fprintf(os.Stderr, "warning: DEX_FUSION_ALPHA=%q is not in (0,1]; using default (0.7)\n", raw)
		return 0.7
	}
	return float32(v)
}

// graphGamma reads DEX_GRAPH_GAMMA — the per-hop decay for the graph lane.
// Zero (unset/invalid) lets the store apply its default (defaultGraphGamma).
// Valid range is (0,1]; out-of-range values are ignored.
func graphGamma() float32 {
	raw := os.Getenv("DEX_GRAPH_GAMMA")
	if raw == "" {
		return 0
	}
	v, err := strconv.ParseFloat(raw, 32)
	if err != nil || v <= 0 || v > 1 {
		fmt.Fprintf(os.Stderr, "warning: DEX_GRAPH_GAMMA=%q is not in (0,1]; using default\n", raw)
		return 0
	}
	return float32(v)
}

// graphHopCap reads DEX_GRAPH_HOP_CAP — the spreading-activation depth.
// Zero (unset/invalid) lets the store apply its default (defaultGraphHopCap).
func graphHopCap() int {
	raw := os.Getenv("DEX_GRAPH_HOP_CAP")
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		fmt.Fprintf(os.Stderr, "warning: DEX_GRAPH_HOP_CAP=%q is not a positive integer; using default\n", raw)
		return 0
	}
	return n
}

// graphLaneWeight reads DEX_GRAPH_WEIGHT — flat multiplier on the graph-proximity lane.
// Zero (unset/invalid) lets the store apply its default (defaultGraphLaneWeight = 1.0).
// Must be > 0; out-of-range values are ignored.
func graphLaneWeight() float32 {
	raw := os.Getenv("DEX_GRAPH_WEIGHT")
	if raw == "" {
		return 0
	}
	v, err := strconv.ParseFloat(raw, 32)
	if err != nil || v <= 0 {
		fmt.Fprintf(os.Stderr, "warning: DEX_GRAPH_WEIGHT=%q is not a positive number; using default\n", raw)
		return 0
	}
	return float32(v)
}

func parseDuration(envVar, raw string, def time.Duration) time.Duration {
	d, err := time.ParseDuration(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %s=%q is not a Go duration; using %s\n", envVar, raw, def)
		return def
	}
	return d
}

// maxHitsPerFile reads DEX_MAX_HITS_PER_FILE from the environment.
// Zero means no per-file cap (default). Positive values enforce result
// diversity — useful when a single heavily-matched file would otherwise
// dominate the top-k results.
func maxHitsPerFile() int {
	raw := os.Getenv("DEX_MAX_HITS_PER_FILE")
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		fmt.Fprintf(os.Stderr, "warning: DEX_MAX_HITS_PER_FILE=%q is not a non-negative integer; ignoring\n", raw)
		return 0
	}
	return n
}

func openStore(ctx context.Context, dbPath string) (*store.Store, error) {
	return store.OpenWith(ctx, dbPath, storeOpts())
}

// cliLogger returns a stderr text logger. Used for the CLI commands
// (index/watch) so verbose output goes to stderr without polluting
// stdout (which the MCP server uses for JSON-RPC).
func cliLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// indexProgressPrinter returns an index.Options.Progress callback that writes
// human-readable progress to stderr (text mode) or NDJSON to stdout (json mode).
// Returns nil when no TTY is attached and format is text — avoids cluttering
// piped output with carriage-return lines.
func indexProgressPrinter(format string) func(phase string, done, total int) {
	if format == "json" {
		return func(phase string, done, total int) {
			pct := 0.0
			if total > 0 {
				pct = float64(done) / float64(total)
			}
			_ = json.NewEncoder(os.Stdout).Encode(struct {
				Type     string  `json:"type"`
				Phase    string  `json:"phase"`
				Done     int     `json:"done"`
				Total    int     `json:"total"`
				Progress float64 `json:"progress"`
			}{"progress", phase, done, total, pct})
		}
	}
	// Text mode: only emit when stderr is a terminal.
	fi, err := os.Stderr.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return nil
	}
	var lastPhase string
	return func(phase string, done, total int) {
		if phase != lastPhase {
			if lastPhase != "" {
				fmt.Fprint(os.Stderr, "\r\033[K") // clear the in-progress line
			}
			lastPhase = phase
		}
		switch phase {
		case "walk":
			fmt.Fprintf(os.Stderr, "\r  walk: %d files scanned", done)
		case "embed":
			if total > 0 {
				pct := done * 100 / total
				fmt.Fprintf(os.Stderr, "\r  embedding: %d/%d chunks (%d%%)", done, total, pct)
			} else {
				fmt.Fprintf(os.Stderr, "\r  embedding: %d chunks", done)
			}
			if done >= total && total > 0 {
				fmt.Fprint(os.Stderr, "\r\033[K") // clear when done
			}
		}
	}
}

// warnIfNoInclude prints a prominent notice when a project has no
// `index.include` in .dex/config.yml. Indexing is opt-in, so without
// it the run produces an empty index — surface that instead of letting
// it look like a silent success.
func warnIfNoInclude(ig *ignore.Matcher, root string) {
	if !ig.IncludeConfigured() {
		fmt.Fprintf(os.Stderr,
			"⚠ no index.include in %s/.dex/config.yml — nothing will be indexed\n", root)
	}
}

// isStaleEmbed reports whether the index's recorded embed model is known to
// differ from the active model. It only compares against the explicit
// DEX_EMBED_MODEL env var — no network calls. Returns false when either side
// is unknown (empty), so pre-migration indexes are never falsely flagged.
func isStaleEmbed(indexModel string) bool {
	active := os.Getenv("DEX_EMBED_MODEL")
	return active != "" && indexModel != "" && active != indexModel
}

// newEmbedClient constructs an embed.Embedder from env vars, falling back to
// indexModel (the model recorded in the target index) when DEX_EMBED_MODEL is
// unset. Callers that have an open *store.Store should pass st.EmbedModel();
// callers that are building a fresh index (or have no store context) pass "".
// If DEX_EMBED_DIM is set, the returned embedder truncates vectors to that
// many dimensions and re-normalises (Matryoshka truncation).
func newEmbedClient(indexModel string) embed.Embedder {
	// Engine selection is explicit and static per index (no hot swap):
	// ONNX vectors live in a different space than ollama/http vectors, so an
	// index must be built and queried with the same engine. DEX_EMBED_ENGINE
	// defaults to "http" (the OpenAI-compatible backend). "onnx" selects the
	// in-process engine, which is only linked in -tags onnx builds (otherwise
	// embed.NewONNX returns a clear "rebuild with -tags onnx" error).
	if indexModel == "bm25-only" {
		return nil
	}
	switch strings.ToLower(os.Getenv("DEX_EMBED_ENGINE")) {
	case "onnx":
		return newONNXEmbedder()
	case "none":
		// Lean / zero-infra profile (#290): no embedder is wired at all.
		// Returns a nil Embedder so the MCP server omits the embedding-backed
		// tools (search_semantic/search_similar/search_context/ctx_overview/
		// search_workspace) and ask degrades to the symbol + graph + BM25
		// lanes. Explicit declaration, not a startup probe — deterministic
		// under GPU contention. See docs/lean-profile.md.
		fmt.Fprintln(os.Stderr, "dex: DEX_EMBED_ENGINE=none — lean profile, no embedder (BM25 + symbol + graph only)")
		return nil
	}

	url := os.Getenv("DEX_EMBED_URL")
	model := os.Getenv("DEX_EMBED_MODEL")

	if url == "" {
		ensureOllamaRunning() // best-effort: start ollama if installed-but-down
		if om, ok := embed.DetectOllama(context.Background()); ok {
			url = om.URL
			if model == "" {
				model = om.Name
			}
			fmt.Fprintf(os.Stderr, "dex: ollama embed model %q at %s\n", model, url)
		} else {
			url = "http://127.0.0.1:8082"
		}
	}
	// Prefer the index-recorded model over the probe/hard-coded default when
	// DEX_EMBED_MODEL is not set — prevents silent dim mismatches on multi-model
	// setups (e.g. nomic 768d index queried with qwen3 2560d).
	if model == "" && indexModel != "" {
		model = indexModel
		fmt.Fprintf(os.Stderr, "dex: using index-recorded embed model %q\n", model)
	}
	if model == "" {
		model = "Qwen/Qwen3-Embedding-4B"
	}

	batch := 32
	if explicit := os.Getenv("DEX_EMBED_BATCH"); explicit != "" {
		if v, err := strconv.Atoi(explicit); err != nil || v <= 0 {
			fmt.Fprintf(os.Stderr, "warning: DEX_EMBED_BATCH=%q is not a positive integer; using 32\n", explicit)
		} else {
			batch = v
		}
	} else {
		// No explicit batch size — probe VRAM and pick a suitable default.
		if vram := embed.FreeVRAMGB(); vram > 0 {
			batch = embed.BatchSizeForVRAM(vram, 32)
		}
	}
	conc := envInt("DEX_EMBED_CONCURRENCY", 4)
	timeout := parseDuration("DEX_EMBED_TIMEOUT", envOr("DEX_EMBED_TIMEOUT", "60s"), 60*time.Second)
	c := embed.NewWithConcurrency(url, model, batch, conc, timeout)
	return embed.WithDimCap(c, envInt("DEX_EMBED_DIM", 0))
}

// newONNXEmbedder builds the in-process ONNX embedder from operator-provided
// env vars. The engine is opt-in behind -tags onnx; in a default build
// embed.NewONNX returns ErrONNXNotBuilt and we exit with a clear message
// rather than silently degrading (the operator explicitly asked for onnx).
func newONNXEmbedder() embed.Embedder {
	cfg := embed.ONNXConfig{
		ModelPath:       os.Getenv("DEX_ONNX_MODEL"),
		TokenizerPath:   os.Getenv("DEX_ONNX_TOKENIZER"),
		LibPath:         os.Getenv("DEX_ONNXRUNTIME_LIB"),
		ModelID:         envOr("DEX_ONNX_MODEL_ID", "model"),
		Dim:             envInt("DEX_ONNX_DIM", 0),
		MaxSeqLen:       envInt("DEX_ONNX_MAX_SEQ", 512),
		Batch:           envInt("DEX_EMBED_BATCH", 32),
		InputIDsName:    os.Getenv("DEX_ONNX_INPUT_IDS"),
		AttentionName:   os.Getenv("DEX_ONNX_ATTENTION"),
		TokenTypeName:   os.Getenv("DEX_ONNX_TOKEN_TYPE"),
		OutputName:      os.Getenv("DEX_ONNX_OUTPUT"),
		NeedsTokenTypes: envBool("DEX_ONNX_TOKEN_TYPES", true),
	}
	em, err := embed.NewONNX(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dex: onnx embed engine: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "dex: onnx embed engine %q (dim %d) from %s\n", em.ModelName(), cfg.Dim, cfg.ModelPath)
	return em
}

func newChatClient() *chat.Client {
	url := os.Getenv("DEX_CHAT_URL")
	model := os.Getenv("DEX_CHAT_MODEL")

	if url == "" {
		ensureOllamaRunning() // best-effort: start ollama if installed-but-down
		if om, ok := embed.DetectOllamaChat(context.Background()); ok {
			url = om.URL
			if model == "" {
				model = om.Name
			}
			fmt.Fprintf(os.Stderr, "dex: ollama chat model %q at %s\n", model, url)
		} else {
			url = "http://127.0.0.1:8081"
		}
	}
	if model == "" {
		model = "Qwen/Qwen2.5-Coder-7B-Instruct"
	}
	timeout := parseDuration("DEX_CHAT_TIMEOUT", envOr("DEX_CHAT_TIMEOUT", "120s"), 120*time.Second)
	return chat.New(url, model, timeout)
}

// newRerankClient returns a rerank.HealthChecker (either the Cohere-compatible
// *rerank.Client or the decoder-style *rerank.ChatReranker), or nil when
// reranking is disabled. Rerank is OFF by default — empty DEX_RERANK_URL
// or DEX_DISABLE_RERANK=1 yields nil; store.Search treats nil as
// "skip the stage".
//
// DEX_RERANK_STYLE selects the backend:
//
//	"chat-vllm" (default) — vLLM + Qwen3-Reranker with <think> assistant prefill
//	"chat"               — chat-completions + logprobs (ollama / standard chat servers)
//	"cohere"             — Cohere-compatible /rerank endpoint (TEI, Infinity, vLLM
//	                       with a cross-encoder model like bge-reranker-v2-m3)
func newRerankClient() rerank.HealthChecker {
	url := os.Getenv("DEX_RERANK_URL")
	if url == "" {
		return nil
	}
	if os.Getenv("DEX_DISABLE_RERANK") == "1" {
		return nil
	}
	model := envOr("DEX_RERANK_MODEL", "Qwen/Qwen3-Reranker-4B")
	timeout := parseDuration("DEX_RERANK_TIMEOUT", envOr("DEX_RERANK_TIMEOUT", "5s"), 5*time.Second)
	style := envOr("DEX_RERANK_STYLE", "chat-vllm")
	if style == "chat" || style == "chat-vllm" {
		rawConc := envOr("DEX_RERANK_CONCURRENCY", "16")
		concurrency, cerr := strconv.Atoi(rawConc)
		if cerr != nil || concurrency <= 0 {
			fmt.Fprintf(os.Stderr, "warning: DEX_RERANK_CONCURRENCY=%q is not a positive integer; using 16\n", rawConc)
			concurrency = 16
		}
		c := rerank.NewChat(url, model, concurrency, timeout)
		// chat-vllm: enable <think> assistant prefill for Qwen3-Reranker on vLLM.
		// Plain chat (ollama / standard servers) must NOT set this — they continue
		// the XML pattern and generate "<" instead of "yes"/"no".
		c.ThinkingPrefill = style == "chat-vllm"
		return c
	}
	return rerank.New(url, model, timeout)
}

// newExpandClient builds the query-side expansion client (#252). Expansion is
// opt-in: with DEX_EXPAND_MODEL unset it returns nil and the feature is a
// no-op. The endpoint defaults to the resolved chat backend (base) so on the
// standard local-ollama deployment, setting just DEX_EXPAND_MODEL=qwen3:4b is
// enough to enable it; DEX_EXPAND_URL overrides the endpoint.
func newExpandClient(base *chat.Client) *chat.Client {
	model := os.Getenv("DEX_EXPAND_MODEL")
	if model == "" {
		return nil
	}
	url := os.Getenv("DEX_EXPAND_URL")
	if url == "" && base != nil {
		url = base.BaseURL
	}
	if url == "" {
		return nil
	}
	timeout := parseDuration("DEX_EXPAND_TIMEOUT", envOr("DEX_EXPAND_TIMEOUT", "5s"), 5*time.Second)
	return chat.New(url, model, timeout)
}

// expandDefaultMode resolves the server-side default expand level. With an
// expansion client configured but DEX_EXPAND_MODE unset, expansion defaults
// to "on" (cheap lexical expansion, no extra embed); otherwise it honors the
// env value, and "off" when no client is wired.
func expandDefaultMode(client *chat.Client) string {
	if client == nil {
		return "off"
	}
	return envOr("DEX_EXPAND_MODE", "on")
}

// envInt reads a positive integer env var with a default.
// Non-positive or unparsable values fall back to def with a warning.
func envInt(name string, def int) int {
	raw := os.Getenv(name)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		fmt.Fprintf(os.Stderr, "warning: %s=%q is not a non-negative integer; using %d\n", name, raw, def)
		return def
	}
	return n
}

// rerankPool reads the candidate-pool cap from the environment.
// Default 40, clamped to [1, 100]. Larger = better recall but slower
// cross-encoder call. Only consulted when a Reranker is wired.
func rerankPool() int {
	raw := envOr("DEX_RERANK_POOL", "40")
	pool, err := strconv.Atoi(raw)
	if err != nil || pool <= 0 {
		fmt.Fprintf(os.Stderr, "warning: DEX_RERANK_POOL=%q is not a positive integer; using 40\n", raw)
		pool = 40
	}
	if pool > 100 {
		pool = 100
	}
	return pool
}

// ─── index ─────────────────────────────────────────────────────────────────

// acquireProjectLock takes the per-project indexer lock. cmdName labels
// the holder ("index"/"reindex"/"watch") and phase reports
// the current pipeline stage. wait blocks until the lock is free;
// breakLock discards an existing lockfile (only safe when the prior
// holder is gone — a live flock cannot be stolen).
//
// On contention without --wait or --break-lock, prints a friendly
// "another dex is busy here" line and returns (nil, nil) so the caller
// can exit 0. On any other failure, returns the error.
func acquireProjectLock(ctx context.Context, p *proj.Project, cmdName, phase string, wait, breakLock bool) (*lock.Lock, error) {
	host, _ := os.Hostname()
	h := lock.Holder{
		PID:     os.Getpid(),
		Host:    host,
		Command: cmdName,
		Phase:   phase,
		Started: time.Now(),
	}
	if breakLock {
		return lock.Steal(p.LockPath, h)
	}
	if wait {
		return lock.AcquireWait(ctx, p.LockPath, h)
	}
	l, err := lock.Acquire(p.LockPath, h)
	if err == nil {
		return l, nil
	}
	if !errors.Is(err, lock.ErrLocked) {
		return nil, err
	}
	holder, _ := lock.ReadHolder(p.LockPath)
	fmt.Fprintf(os.Stderr, "another dex indexer is running on %s%s\n", p.Root, describeHolder(holder))
	fmt.Fprintln(os.Stderr, "  pass --wait to block, or --break-lock if the holder is gone")
	return nil, nil
}

// describeHolder formats a parenthetical for the contention message.
// Returns "" when no holder info is available.
func describeHolder(h *lock.Holder) string {
	if h == nil {
		return ""
	}
	var parts []string
	if h.PID != 0 {
		parts = append(parts, fmt.Sprintf("pid %d", h.PID))
	}
	if h.Command != "" {
		parts = append(parts, fmt.Sprintf("cmd=%s", h.Command))
	}
	if h.Phase != "" {
		parts = append(parts, fmt.Sprintf("phase=%s", h.Phase))
	}
	if !h.Started.IsZero() {
		parts = append(parts, fmt.Sprintf("for %s", time.Since(h.Started).Round(time.Second)))
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

// clearCacheKeepLock removes everything inside p.CacheDir except the
// lock file. Used by `reindex` so the indexer lock can be acquired
// before the destructive sweep without removing the lockfile under
// our own feet.
func clearCacheKeepLock(p *proj.Project) error {
	entries, err := os.ReadDir(p.CacheDir)
	if err != nil {
		return err
	}
	lockBase := filepath.Base(p.LockPath)
	for _, e := range entries {
		if e.Name() == lockBase {
			continue
		}
		if err := os.RemoveAll(filepath.Join(p.CacheDir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// cmdIndexDispatch peels off the `status` subcommand before
// falling through to `cmdIndex` (which expects a single path arg).
func cmdIndexDispatch(ctx context.Context, args []string) error {
	if len(args) >= 1 {
		switch args[0] {
		case "status":
			return cmdIndexStatus(ctx, args[1:])
		}
	}
	return cmdIndex(ctx, args)
}

func cmdIndex(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("index", flag.ContinueOnError)
	setHelp(fs,
		"Build or refresh the per-project index (chunks + Go static graph).",
		"dex index [flags] <path>")
	verbose := fs.Bool("v", false, "verbose")
	force := fs.Bool("force", false, "bypass protected-path and git-tree guards")
	dryRun := fs.Bool("dry-run", false, "walk the file tree and show what would be indexed, without writing to the index")
	graphMode := fs.String("graph", "on", "graph phase: on|off|only ('on' runs both phases, 'off' skips graph, 'only' skips chunk/embed and just refreshes the graph)")
	format := fs.String("format", "text", "output format: text|json")
	waitLock := fs.Bool("wait", false, "if another dex indexer is running on this project, wait for it to finish instead of skipping")
	breakLock := fs.Bool("break-lock", false, "discard an existing project lockfile (use only when the prior holder is gone)")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	switch *graphMode {
	case "on", "off", "only":
	default:
		return fmt.Errorf("invalid --graph=%s (want on|off|only)", *graphMode)
	}
	switch *format {
	case "text", "json":
	default:
		return fmt.Errorf("unknown --format=%s (want text|json)", *format)
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return fmt.Errorf("index needs exactly one path argument")
	}
	base, err := indexDir()
	if err != nil {
		return err
	}
	p, err := proj.Resolve(rest[0], base)
	if err != nil {
		return err
	}
	if err := proj.CheckIndexable(p, *force); err != nil {
		return err
	}
	ig, err := ignore.New(p.Root)
	if err != nil {
		return err
	}
	if *dryRun {
		warnIfNoInclude(ig, p.Root)
		return runIndexDryRun(ctx, p, ig, *verbose, *format)
	}
	if err := p.EnsureCacheDir(); err != nil {
		return err
	}
	lk, err := acquireProjectLock(ctx, p, "index", "chunk", *waitLock, *breakLock)
	if err != nil {
		return err
	}
	if lk == nil {
		return nil // another indexer is running; message already printed
	}
	defer func() { _ = lk.Release() }()
	st, err := openStore(ctx, p.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	// Phase 1: chunk + embed (skipped when --graph=only).
	if *graphMode != "only" {
		warnIfNoInclude(ig, p.Root)
		opts := index.Options{
			Verbose:     *verbose,
			Logger:      cliLogger(),
			Concurrency: envInt("DEX_INDEX_CONCURRENCY", 0),
			Progress:    indexProgressPrinter(*format),
		}
		ix := index.New(p, st, newEmbedClient(st.EmbedModel()), ig, opts)
		if err := ix.Run(ctx); err != nil {
			return err
		}
	}
	if err := st.SetProjectRoot(ctx, p.Root); err != nil {
		return err
	}

	// Phase 2: graph extraction (skipped when --graph=off).
	// In --graph=only mode the user explicitly asked for the graph, so a
	// failure is hard. In default mode the chunk phase has already
	// succeeded, so we warn-and-continue — losing the graph shouldn't
	// invalidate a fresh embed pass.
	var gstats *graph.Stats
	if *graphMode != "off" {
		_ = lk.SetPhase("graph")
		s, gerr := runGraphPhase(ctx, p, st, *verbose)
		if gerr != nil {
			if *graphMode == "only" {
				return gerr
			}
			fmt.Fprintf(os.Stderr, "⚠ graph phase failed: %v (chunk index is still usable)\n", gerr)
		} else {
			gstats = s
		}
	}

	// Phase 2.5: embed graph nodes (symbol KNN index).
	// Skipped when: embedder unavailable (lean/none profile) or graph was off.
	if *graphMode != "off" && gstats != nil {
		if em := newEmbedClient(st.EmbedModel()); em != nil {
			_ = lk.SetPhase("graph-embed")
			if n, err := embedGraphNodes(ctx, st, em, *verbose); err != nil {
				fmt.Fprintf(os.Stderr, "⚠ graph-embed phase failed: %v\n", err)
			} else if n > 0 && *verbose {
				fmt.Printf("  [graph-embed] %d nodes embedded\n", n)
			}
		}
	}

	if *graphMode == "only" {
		// Mirror the old `graph index` output shape so existing scripts
		// piping --format=json keep parsing.
		return reportGraphStats(p.Root, gstats, *format)
	}

	stats, err := st.Stats(ctx)
	if err != nil {
		return err
	}
	if *format == "json" {
		return reportIndexResult(p.Root, stats, gstats)
	}
	fmt.Fprintf(os.Stderr, "✓ indexed %s\n", p.Root)
	fmt.Fprintf(os.Stderr, "  chunks: %d  files: %d  dim: %d\n", stats.Chunks, stats.Files, stats.Dim)
	if gstats != nil {
		_ = reportGraphStats(p.Root, gstats, "text")
	}
	return nil
}

// runIndexDryRun walks the file tree applying all filters and prints a report
// of what would be indexed, without writing anything to the store.
func runIndexDryRun(ctx context.Context, p *proj.Project, ig *ignore.Matcher, verbose bool, format string) error {
	type fileEntry struct {
		path   string
		chunks int
	}
	type skipEntry struct {
		path   string
		reason string
	}

	var (
		included    []fileEntry
		skipped     []skipEntry
		skipIgnore  int
		skipBinary  int
		skipSecret  int
		skipSize    int
		totalChunks int
	)

	const maxSize = int64(1 << 20) // 1 MB — mirrors index.Options default

	walkErr := filepath.WalkDir(p.Root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rel, _ := filepath.Rel(p.Root, path)
		if rel == "." {
			return nil
		}
		if ig.Match(rel, d.IsDir()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			skipped = append(skipped, skipEntry{path: rel, reason: "ignored"})
			skipIgnore++
			return nil
		}
		if d.IsDir() {
			gitMarker := filepath.Join(path, ".git")
			if fi, err2 := os.Lstat(gitMarker); err2 == nil && !fi.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if !ignore.IndexableExt(path) && !ignore.IndexableBasename(path) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.Size() > maxSize {
			skipped = append(skipped, skipEntry{path: rel, reason: "too-large"})
			skipSize++
			return nil
		}
		data, err := os.ReadFile(path) //nolint:gosec // symlinks already rejected above
		if err != nil {
			return nil
		}
		if ignore.LooksBinary(data) {
			skipped = append(skipped, skipEntry{path: rel, reason: "binary"})
			skipBinary++
			return nil
		}
		if !ignore.IsTestPath(rel) && ignore.LooksLikeSecret(data) {
			skipped = append(skipped, skipEntry{path: rel, reason: "secret-pattern"})
			skipSecret++
			return nil
		}
		chunks, _ := chunk.Chunks(ctx, rel, data)
		included = append(included, fileEntry{path: rel, chunks: len(chunks)})
		totalChunks += len(chunks)
		return nil
	})
	if walkErr != nil {
		return fmt.Errorf("walk: %w", walkErr)
	}

	totalSkipped := len(skipped)

	if format == "json" {
		type skipBreakdown struct {
			Ignored  int `json:"ignored"`
			Binary   int `json:"binary"`
			Secret   int `json:"secret_pattern"`
			TooLarge int `json:"too_large"`
		}
		type dryRunResult struct {
			Project   string        `json:"project"`
			DryRun    bool          `json:"dry_run"`
			Files     int           `json:"files"`
			Chunks    int           `json:"chunks"`
			Skipped   int           `json:"skipped"`
			Breakdown skipBreakdown `json:"skip_breakdown"`
		}
		return json.NewEncoder(os.Stdout).Encode(dryRunResult{
			Project: p.Root,
			DryRun:  true,
			Files:   len(included),
			Chunks:  totalChunks,
			Skipped: totalSkipped,
			Breakdown: skipBreakdown{
				Ignored:  skipIgnore,
				Binary:   skipBinary,
				Secret:   skipSecret,
				TooLarge: skipSize,
			},
		})
	}

	if verbose {
		for _, f := range included {
			fmt.Printf("  include  %-60s  %d chunks\n", f.path, f.chunks)
		}
		for _, s := range skipped {
			fmt.Printf("  skip     %-60s  %s\n", s.path, s.reason)
		}
		if len(included)+len(skipped) > 0 {
			fmt.Println()
		}
	}

	fmt.Printf("dry-run: %s\n", p.Root)
	fmt.Printf("  would index: %d files  %d chunks\n", len(included), totalChunks)

	if totalSkipped > 0 || skipIgnore > 0 {
		var parts []string
		if skipIgnore > 0 {
			parts = append(parts, fmt.Sprintf("%d ignored", skipIgnore))
		}
		if skipBinary > 0 {
			parts = append(parts, fmt.Sprintf("%d binary", skipBinary))
		}
		if skipSecret > 0 {
			parts = append(parts, fmt.Sprintf("%d secret-pattern", skipSecret))
		}
		if skipSize > 0 {
			parts = append(parts, fmt.Sprintf("%d too-large", skipSize))
		}
		fmt.Printf("  skipped: %d files (%s)\n", totalSkipped, strings.Join(parts, ", "))
	}
	return nil
}

// indexResult is the JSON payload emitted by `index --format=json`
// (combined chunk + graph stats). The Graph field is omitted when
// the graph phase was skipped or failed.
type indexResult struct {
	Project string            `json:"project"`
	Chunks  int               `json:"chunks"`
	Files   int               `json:"files"`
	Dim     int               `json:"dim"`
	Graph   *graphIndexResult `json:"graph,omitempty"`
}

func reportIndexResult(project string, s store.Stats, g *graph.Stats) error {
	out := indexResult{
		Project: project,
		Chunks:  s.Chunks,
		Files:   s.Files,
		Dim:     s.Dim,
	}
	if g != nil {
		out.Graph = &graphIndexResult{
			Project:    project,
			Packages:   g.Packages,
			Nodes:      g.NodesUpserted,
			Edges:      g.EdgesUpserted,
			Pruned:     g.NodesPruned,
			PrunedEdge: g.EdgesPruned,
			Linked:     g.LinkedToChunks,
			ElapsedMS:  g.Elapsed.Milliseconds(),
			Warnings:   g.Warnings,
		}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// ─── search ────────────────────────────────────────────────────────────────

// cmdSearch dispatches `dex search <semantic|symbol>`. Mirrors
// the MCP tool names `search_semantic` / `search_symbol`.
func cmdSearch(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("search needs a subcommand: semantic | symbol")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "semantic":
		return cmdSearchSemantic(ctx, rest)
	case "symbol":
		return cmdSearchSymbol(ctx, rest)
	case "-h", "--help", "help":
		fmt.Fprintln(os.Stderr, `usage:
  dex search semantic <path> <query...>   hybrid top-k chunks (MCP: find)
  dex search symbol   <path> <name>       exact identifier lookup (MCP: lookup)`)
		return nil
	default:
		return fmt.Errorf("unknown search subcommand: %s (have: semantic, symbol)", sub)
	}
}

func cmdSearchSemantic(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("search semantic", flag.ContinueOnError)
	setHelp(fs,
		"Hybrid top-k chunks for a query (MCP: find).",
		"dex search semantic [flags] [<path>] <query...>",
		`dex search semantic . "retry logic"`,
		`dex search semantic . --k=16 --explain "rate limiter"`,
		`dex search semantic . --max-content-bytes=4000 "error handling"`,
	)
	k := fs.Int("k", 8, "number of results to return")
	rerankFlag := fs.String("rerank", "", "set to 'off' to skip the rerank stage for this query (no effect when DEX_RERANK_URL is unset)")
	format := fs.String("format", "text", "output format: text | json")
	explain := fs.Bool("explain", false, "show per-chunk score breakdown and stage timings")
	maxContentBytes := fs.Int("max-content-bytes", 1500, "truncate content display at N bytes (0 = no limit)")
	verbose := fs.Bool("v", false, "verbose: show score breakdown and timing (equivalent to --explain)")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	if *verbose {
		*explain = true
	}
	path, rest := splitProjectArg(fs.Args())
	if len(rest) == 0 {
		return fmt.Errorf("search semantic needs a query (path defaults to cwd)")
	}
	q := strings.Join(rest, " ")
	if strings.TrimSpace(q) == "" {
		return fmt.Errorf("query is empty — pass a natural-language description or code fragment")
	}

	base, err := indexDir()
	if err != nil {
		return err
	}
	p, err := proj.Resolve(path, base)
	if err != nil {
		return err
	}
	if _, err := os.Stat(p.DBPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no index for %s — run `dex index %s` first", p.Root, p.Root)
		}
		return err
	}
	opts := storeOpts()
	if *rerankFlag == "off" {
		opts.Reranker = nil
	}
	st, err := store.OpenWith(ctx, p.DBPath, opts)
	if err != nil {
		return err
	}
	defer st.Close()
	em := newEmbedClient(st.EmbedModel())
	var queryVec []float32
	var embedDur time.Duration
	if em != nil {
		t0 := time.Now()
		vecs, err := em.Embed(ctx, []string{q})
		embedDur = time.Since(t0)
		if err != nil {
			if !errors.Is(err, embed.ErrUnreachable) {
				return err
			}
			// Degrade, don't crash: drop the semantic leg and run BM25-only.
			fmt.Fprintf(os.Stderr,
				"dex: embedding service offline at %s — degraded to BM25-only (start ollama, or set DEX_EMBED_URL)\n",
				em.Endpoint())
		} else {
			queryVec = vecs[0]
		}
	}
	t1 := time.Now()
	hits, err := st.Search(ctx, queryVec, q, *k)
	searchDur := time.Since(t1)
	if err != nil {
		return err
	}
	if len(hits) == 0 {
		fmt.Fprintln(os.Stderr, "no results")
		return nil
	}
	switch *format {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(hitsToJSON(hits))
	case "", "text":
		for i, h := range hits {
			loc := fmt.Sprintf("%s:%d-%d", h.Path, h.StartLine, h.EndLine)
			if h.Name != "" {
				loc = h.Name + "  " + loc
			}
			header := fmt.Sprintf("─── #%d %s  (%s)", i+1, loc, h.Kind)
			if *explain {
				scores := fmt.Sprintf("sem=%.4f", h.Score)
				if h.BM25Score > 0 {
					scores += fmt.Sprintf("  bm25=%.4f", h.BM25Score)
				}
				if h.RRFScore > 0 {
					scores += fmt.Sprintf("  rrf=%.4f", h.RRFScore)
				}
				if h.RerankScore > 0 {
					scores += fmt.Sprintf("  rerank=%.4f", h.RerankScore)
				}
				fmt.Println(header)
				fmt.Println("  " + scores)
			} else {
				header += fmt.Sprintf("  score=%.4f", h.Score)
				if h.RerankScore > 0 {
					header += fmt.Sprintf("  rerank=%.4f", h.RerankScore)
				}
				fmt.Println(header)
			}
			fmt.Println(truncate(h.Content, *maxContentBytes))
			fmt.Println()
		}
		if *explain {
			fmt.Fprintf(os.Stderr, "timing:  embed=%dms  search=%dms  total=%dms\n",
				embedDur.Milliseconds(), searchDur.Milliseconds(), (embedDur + searchDur).Milliseconds())
		}
		return nil
	default:
		return fmt.Errorf("unknown format %q (want text|json)", *format)
	}
}

// cmdSearchSymbol wraps the MCP `search_symbol` tool. Exact identifier
// lookup against the indexed chunks — no embedding required.
func cmdSearchSymbol(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("search symbol", flag.ContinueOnError)
	setHelp(fs,
		"Exact identifier lookup (MCP: lookup).",
		"dex search symbol [flags] [<path>] <name>",
		`dex search symbol . "RateLimiter"`,
		`dex search symbol . --k=20 "func.*Handler"`,
	)
	k := fs.Int("k", 10, "max results to return")
	format := fs.String("format", "text", "output format: text | json")
	maxContentBytes := fs.Int("max-content-bytes", 1500, "truncate content display at N bytes (0 = no limit)")
	_ = fs.Bool("v", false, "verbose (accepted, currently no-op)")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	path, rest := splitProjectArg(fs.Args())
	if len(rest) != 1 {
		if len(rest) == 0 {
			return fmt.Errorf("search symbol needs a name (path defaults to cwd) — e.g. `dex search symbol Watcher`")
		}
		return fmt.Errorf("search symbol takes one <name> (got %d extra args)", len(rest)-1)
	}
	base, err := indexDir()
	if err != nil {
		return err
	}
	p, err := proj.Resolve(path, base)
	if err != nil {
		return err
	}
	s, _ := newServerFromEnv(base)
	out, err := s.FindSymbol(ctx, mcp.FindSymbolInput{
		Name:        rest[0],
		ProjectRoot: p.Root,
		K:           *k,
	})
	if err != nil {
		return err
	}
	if *format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	printSearchHitResult(out.Status, out.Hint, out.Project, out.Hits, *maxContentBytes)
	return nil
}

// ─── ask ──────────────────────────────────────────────────────────────────

// cmdAsk mirrors the `ask` MCP tool so agents and humans share one
// implementation. The flag surface maps 1-to-1 onto mcp.ContextInput
// so a CLI invocation can stand in for a tool call when an agent is
// offline.
func cmdAsk(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("ask", flag.ContinueOnError)
	setHelp(fs,
		"One-shot router — composes semantic + symbol + graph; emit suggested_reads + next_action. Use this BEFORE grep loops.",
		"dex ask [flags] [<path>] <question...>",
		`dex ask . "where is the rate limiter?"`,
		`dex ask . --intent symbol_lookup "RateLimiter"`,
		`dex ask . --format json "retry logic" | jq .suggested_reads`,
	)
	intent := fs.String("intent", "", "force a strategy: auto|behavior_search|symbol_lookup|callers|callees|architecture|package_topology|editing_context")
	k := fs.Int("k", 8, "max hits per lane (capped at 30)")
	format := fs.String("format", "text", "output format: text | json")
	noInline := fs.Bool("no-inline", false, "skip inlining raw file contents into suggested_reads (stored chunk/file summaries are still emitted; use --format=json to inspect)")
	maxContentBytes := fs.Int("max-content-bytes", 0, "truncate content display at N bytes (0 = no limit; applies to text output only)")
	verbose := fs.Bool("v", false, "verbose: show wall-clock timing")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	if !validIntent(*intent) {
		return fmt.Errorf("invalid --intent=%q (want one of: auto, behavior_search, symbol_lookup, callers, callees, architecture, package_topology, editing_context)", *intent)
	}
	path, rest := splitProjectArg(fs.Args())
	if len(rest) == 0 {
		return fmt.Errorf("ask needs a question (path defaults to cwd) — e.g. `dex ask \"where is the watcher?\"`")
	}
	question := strings.Join(rest, " ")
	if strings.TrimSpace(question) == "" {
		return fmt.Errorf("question is empty — pass a natural-language description")
	}

	base, err := indexDir()
	if err != nil {
		return err
	}
	p, err := proj.Resolve(path, base)
	if err != nil {
		return err
	}

	s, _ := newServerFromEnv(base)
	t0 := time.Now()
	_, out, err := s.ContextRouter(ctx, mcp.ContextInput{
		Project:  p.Root,
		Question: question,
		Intent:   *intent,
		K:        *k,
		NoInline: *noInline,
	})
	elapsed := time.Since(t0)
	if err != nil {
		return err
	}

	switch *format {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	case "", "text":
		printContextText(out, *maxContentBytes)
		if *verbose {
			fmt.Fprintf(os.Stderr, "timing: %dms\n", elapsed.Milliseconds())
		}
		return nil
	default:
		return fmt.Errorf("unknown format %q (want text|json)", *format)
	}
}

// ─── view ──────────────────────────────────────────────────────────────────

// cmdView dispatches `dex view <summarize>`. Mirrors the MCP
// `file_view` tool by parking it under a `view` group so future
// view-style ops (e.g. `view chunk`) land next to it.
func cmdView(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("view needs a subcommand: summarize")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "summarize":
		return cmdViewSummarize(ctx, rest)
	case "-h", "--help", "help":
		fmt.Fprintln(os.Stderr, `usage:
  dex view summarize <path> <file>   summarize a file slice (MCP: read)`)
		return nil
	default:
		return fmt.Errorf("unknown view subcommand: %s (have: summarize)", sub)
	}
}

func cmdViewSummarize(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("view summarize", flag.ContinueOnError)
	setHelp(fs,
		"Summarize a file slice via the chat model (MCP: read).",
		"dex view summarize [flags] [<path>] <file>")
	start := fs.Int("start", 0, "first line to summarize (1-indexed; 0 = beginning of file)")
	end := fs.Int("end", 0, "last line to summarize (1-indexed, inclusive; 0 = end of file)")
	focus := fs.String("focus", "", "optional steering — e.g. 'public API surface', 'side effects'")
	temp := fs.Float64("temperature", 0, "sampling temperature (0 = server default)")
	maxTok := fs.Int("max-tokens", 0, "max tokens to generate (0 = server default)")
	format := fs.String("format", "text", "output format: text | json")
	verbose := fs.Bool("v", false, "include model name and other debug headers in text output")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	path, rest := splitProjectArg(fs.Args())
	if len(rest) != 1 {
		if len(rest) == 0 {
			return fmt.Errorf("view summarize needs a <file> (path defaults to cwd)")
		}
		return fmt.Errorf("view summarize takes one <file> (got %d extra args)", len(rest)-1)
	}
	base, err := indexDir()
	if err != nil {
		return err
	}
	p, err := proj.Resolve(path, base)
	if err != nil {
		return err
	}
	s, _ := newServerFromEnv(base)
	out, err := s.Summarize(ctx, mcp.SummarizeInput{
		Path:        rest[0],
		ProjectRoot: p.Root,
		StartLine:   *start,
		EndLine:     *end,
		Focus:       *focus,
		Temperature: float32(*temp),
		MaxTokens:   *maxTok,
	})
	if err != nil {
		return err
	}
	if *format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	if out.Status != "ok" {
		fmt.Fprintf(os.Stderr, "status: %s\n", out.Status)
		if out.Hint != "" {
			fmt.Fprintf(os.Stderr, "hint:   %s\n", out.Hint)
		}
		return nil
	}
	fmt.Printf("file:  %s (lines %d-%d, %d bytes", out.Path, out.StartLine, out.EndLine, out.Bytes)
	if out.Truncated {
		fmt.Print(", truncated")
	}
	fmt.Println(")")
	if *verbose && out.Model != "" {
		fmt.Printf("model: %s\n", out.Model)
	}
	fmt.Println()
	fmt.Println(out.Content)
	if out.FinishReason != "" && out.FinishReason != "stop" {
		fmt.Fprintf(os.Stderr, "\n(finish_reason=%s)\n", out.FinishReason)
	}
	return nil
}

// ─── generate ──────────────────────────────────────────────────────────────

func cmdGenerate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	setHelp(fs,
		"Generate code grounded in the project's index (RAG: top-k chunks → chat endpoint).",
		"dex generate [flags] [<path>] <prompt...>")
	k := fs.Int("k", 8, "number of RAG chunks to retrieve as context")
	noRAG := fs.Bool("no-rag", false, "skip retrieval; send prompt to the chat endpoint without project context")
	system := fs.String("system", "", "override the default system prompt")
	temp := fs.Float64("temperature", 0, "sampling temperature (0 = server default)")
	maxTok := fs.Int("max-tokens", 0, "max tokens to generate (0 = server default)")
	showCtx := fs.Bool("show-context", false, "print the chunks fed as context before the model output")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	path, rest := splitProjectArg(fs.Args())
	if len(rest) == 0 {
		return fmt.Errorf("generate needs a prompt (path defaults to cwd)")
	}
	prompt := strings.Join(rest, " ")
	if strings.TrimSpace(prompt) == "" {
		return fmt.Errorf("prompt is empty")
	}

	base, err := indexDir()
	if err != nil {
		return err
	}
	p, err := proj.Resolve(path, base)
	if err != nil {
		return err
	}

	var hits []store.Hit
	if !*noRAG {
		if _, err := os.Stat(p.DBPath); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("no index for %s — run `dex index %s` first, or pass --no-rag to skip retrieval", p.Root, p.Root)
			}
			return err
		}
		st, err := openStore(ctx, p.DBPath)
		if err != nil {
			return err
		}
		em := newEmbedClient(st.EmbedModel())
		if em == nil {
			st.Close()
			return fmt.Errorf("RAG requires an embedding model; re-run with --no-rag or configure DEX_EMBED_URL")
		}
		vecs, err := em.Embed(ctx, []string{prompt})
		if err != nil {
			st.Close()
			return fmt.Errorf("embed: %w", err)
		}
		hits, err = st.Search(ctx, vecs[0], prompt, *k)
		st.Close()
		if err != nil {
			return fmt.Errorf("search: %w", err)
		}
	}

	sysPrompt := *system
	if strings.TrimSpace(sysPrompt) == "" {
		sysPrompt = "You are a precise coding assistant. " +
			"When CONTEXT chunks from the user's project are provided, ground your answer in them — " +
			"reference real symbols, paths, and conventions rather than inventing names. " +
			"Output code in fenced blocks; keep prose minimal."
	}

	userContent := prompt
	if len(hits) > 0 {
		userContent = store.FormatHits(hits) + "\n\n---\n\n" + prompt
	}

	if *showCtx && len(hits) > 0 {
		fmt.Fprintln(os.Stderr, "─── context fed to the model ───")
		for i, h := range hits {
			fmt.Fprintf(os.Stderr, "#%d %s:%d-%d  (%s)  score=%.4f\n", i+1, h.Path, h.StartLine, h.EndLine, h.Kind, h.Score)
		}
		fmt.Fprintln(os.Stderr, "────────────────────────────────")
	}

	cc := newChatClient()
	resp, err := cc.Generate(ctx, []chat.Message{
		{Role: "system", Content: sysPrompt},
		{Role: "user", Content: userContent},
	}, chat.Options{
		Temperature: float32(*temp),
		MaxTokens:   *maxTok,
	})
	if err != nil {
		return err
	}
	fmt.Println(resp.Content)
	if resp.FinishReason != "" && resp.FinishReason != "stop" {
		fmt.Fprintf(os.Stderr, "\n(finish_reason=%s)\n", resp.FinishReason)
	}
	return nil
}

// ─── index status ──────────────────────────────────────────────────────────

func cmdIndexStatus(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("index status", flag.ContinueOnError)
	setHelp(fs,
		"Show endpoint health and project stats (chunks/files/graph). Optional path narrows to one project. (MCP: status)",
		"dex index status [<path>]")
	format := fs.String("format", "text", "output format: text|json")
	jsonFlag := fs.Bool("json", false, "shorthand for --format=json")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	if *jsonFlag {
		*format = "json"
	}
	if *format != "text" && *format != "json" {
		return fmt.Errorf("unknown --format=%s (want text|json)", *format)
	}
	rest := fs.Args()
	checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	base, err := indexDir()
	if err != nil {
		return err
	}

	if len(rest) == 1 {
		// Per-project status
		p, err := proj.Resolve(rest[0], base)
		if err != nil {
			return err
		}
		if _, err := os.Stat(p.DBPath); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				if *format == "json" {
					return json.NewEncoder(os.Stdout).Encode(map[string]any{"project": p.Root, "status": "not-indexed"})
				}
				fmt.Printf("\n%s\n  not indexed — run: dex index %s\n", p.Root, p.Root)
				return nil
			}
			return err
		}
		st, err := openStore(ctx, p.DBPath)
		if err != nil {
			return err
		}
		defer st.Close()
		stats, err := st.Stats(ctx)
		if err != nil {
			return err
		}
		nodes, edges, _ := st.GraphStats(ctx)
		stale := isStaleEmbed(stats.EmbedModel)
		if *format == "json" {
			out := map[string]any{
				"project":    p.Root,
				"status":     "ok",
				"files":      stats.Files,
				"chunks":     stats.Chunks,
				"dim":        stats.Dim,
				"nodes":      nodes,
				"edges":      edges,
				"last_index": stats.LastIndex,
			}
			if stale {
				out["stale"] = true
			}
			return json.NewEncoder(os.Stdout).Encode(out)
		}
		// Header line up top groups version + index dir so the rest of
		// the output reads as content under a single banner instead of
		// orphaned bits between sections.
		fmt.Printf("dex %s · %s\n\n", mcp.Version, base)
		printEndpoints(checkCtx)
		fmt.Println()
		fmt.Printf("  %s\n", p.Root)
		printProjectStatLines("    ", projectStats{
			lastIndex: stats.LastIndex,
			files:     stats.Files,
			chunks:    stats.Chunks,
			nodes:     nodes,
			edges:     edges,
			dim:       stats.Dim,
			stale:     stale,
		})
		// Action hints only on the per-project view — the
		// multi-project listing keeps the per-block content uniform.
		if stale {
			fmt.Printf("    → embed model changed — run: dex reindex %s\n", p.Root)
		}
		if !stats.LastIndex.IsZero() && time.Since(stats.LastIndex) > 24*time.Hour {
			fmt.Printf("    → stale — run: dex index %s\n", p.Root)
		}
		return nil
	}
	if *format == "text" {
		// Header only in text mode
		fmt.Printf("dex %s · %s\n\n", mcp.Version, base)
		printEndpoints(checkCtx)
		fmt.Println()
	}

	// All-project summary
	entries, err := os.ReadDir(base)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Printf("\nindex dir: %s\nno projects indexed yet\n", base)
			return nil
		}
		return err
	}

	type row struct {
		root      string
		cacheHash string // first 5 chars of cache dir name; used for untagged display
		chunks    int
		files     int
		nodes     int64
		edges     int64
		last      time.Time
		stale     bool
		corrupt   bool
		empty     bool
	}
	results := make([]row, len(entries))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, e := range entries {
		if !e.IsDir() {
			continue
		}
		dbPath := filepath.Join(base, e.Name(), "index.db")
		if _, err := os.Stat(dbPath); err != nil {
			continue
		}
		wg.Add(1)
		go func(idx int, name, path string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			// openStore (not store.Open) so DEX_VECTOR_QUANT is honored —
			// a default-Options open resolves quant to float32 and would make
			// this read-only listing drop+rebuild chunk_vecs on an int8 index
			// (#334).
			st, err := openStore(ctx, path)
			if err != nil {
				results[idx] = row{root: fmt.Sprintf("(corrupt cache: %s)", name), corrupt: true}
				return
			}
			stats, _ := st.Stats(ctx)
			root, _ := st.ProjectRoot(ctx)
			nodes, edges, _ := st.GraphStats(ctx)
			st.Close()
			if stats.Chunks == 0 {
				results[idx] = row{empty: true}
				return
			}
			cacheHash := name
			if len(cacheHash) > 5 {
				cacheHash = cacheHash[:5]
			}
			if root == "" {
				root = fmt.Sprintf("untagged (%s…)", cacheHash)
			}
			results[idx] = row{
				root:      root,
				cacheHash: cacheHash,
				chunks:    stats.Chunks,
				files:     stats.Files,
				nodes:     nodes,
				edges:     edges,
				last:      stats.LastIndex,
				stale:     isStaleEmbed(stats.EmbedModel),
			}
		}(i, e.Name(), dbPath)
	}
	wg.Wait()

	var rows []row
	var empties int
	for _, r := range results {
		switch {
		case r.empty:
			empties++
		case r.root != "":
			rows = append(rows, r)
		}
	}

	if len(rows) == 0 && empties == 0 {
		if *format == "json" {
			return json.NewEncoder(os.Stdout).Encode(map[string]any{"projects": []any{}})
		}
		fmt.Println("projects (0 indexed)")
		fmt.Println("  no projects indexed yet — run: dex index <path>")
		return nil
	}

	// Sort by recency descending. Zero timestamps sink to the bottom
	// so genuinely-stale and unidentifiable indexes don't fight the
	// fresh ones for screen space.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].last.IsZero() != rows[j].last.IsZero() {
			return !rows[i].last.IsZero()
		}
		return rows[i].last.After(rows[j].last)
	})

	if *format == "json" {
		type jsonRow struct {
			Project   string    `json:"project"`
			Files     int       `json:"files"`
			Chunks    int       `json:"chunks"`
			Nodes     int64     `json:"nodes"`
			Edges     int64     `json:"edges"`
			LastIndex time.Time `json:"last_index"`
			Corrupt   bool      `json:"corrupt,omitempty"`
			Stale     bool      `json:"stale,omitempty"`
		}
		out := make([]jsonRow, 0, len(rows))
		for _, r := range rows {
			jr := jsonRow{
				Project:   r.root,
				Files:     r.files,
				Chunks:    r.chunks,
				Nodes:     r.nodes,
				Edges:     r.edges,
				LastIndex: r.last,
				Corrupt:   r.corrupt,
				Stale:     r.stale,
			}
			out = append(out, jr)
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"projects": out})
	}

	fmt.Printf("projects (%d indexed)\n", len(rows))

	// Stacked layout: each project is a self-contained block of
	// labelled key:value rows. The labels are padded to a fixed width
	// so values line up vertically inside each block. We don't try to
	// align the value column ACROSS blocks — different projects have
	// different stat counts and aligning across them gives up
	// scannability inside a single block for nothing useful.
	for i, r := range rows {
		if i > 0 {
			fmt.Println()
		}
		if r.corrupt {
			fmt.Printf("  %s\n    CORRUPT\n", r.root)
			continue
		}
		fmt.Printf("  %s\n", r.root)
		printProjectStatLines("    ", projectStats{
			lastIndex: r.last,
			files:     r.files,
			chunks:    r.chunks,
			nodes:     r.nodes,
			edges:     r.edges,
			stale:     r.stale,
		})
	}
	if empties > 0 {
		noun := "index"
		if empties != 1 {
			noun = "indexes"
		}
		fmt.Printf("\n  (%d empty %s skipped)\n", empties, noun)
	}
	return nil
}

// ─── nuke ──────────────────────────────────────────────────────────────────

func cmdNuke(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("nuke", flag.ContinueOnError)
	setHelp(fs,
		"Delete the on-disk index for a project (irreversible). Prompts on a TTY; non-interactive callers must pass --yes.",
		"dex nuke [--yes] <path>")
	yes := fs.Bool("yes", false, "skip the interactive prompt (required when stdin is not a terminal)")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return fmt.Errorf("nuke needs exactly one path argument")
	}
	base, err := indexDir()
	if err != nil {
		return err
	}
	p, err := proj.Resolve(rest[0], base)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		// Path is gone — compute the cache key directly from the supplied
		// string (no realpath resolution possible) and fall through.
		p, err = proj.ResolveDeleted(rest[0], base)
		if err != nil {
			return err
		}
	}
	if _, err := os.Stat(p.CacheDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Printf("nothing to remove: no index for %s\n", p.Root)
			return nil
		}
		return err
	}
	if !*yes {
		if !stdinIsTTY() {
			return fmt.Errorf("refusing to nuke without --yes: stdin is not a terminal (would be silent in scripts)")
		}
		fmt.Fprintf(os.Stderr, "About to delete index for %s\n  cache: %s\nThis is irreversible. Continue? [y/N] ", p.Root, p.CacheDir)
		reader := bufio.NewReader(os.Stdin)
		line, _ := reader.ReadString('\n')
		ans := strings.TrimSpace(strings.ToLower(line))
		if ans != "y" && ans != "yes" {
			fmt.Fprintln(os.Stderr, "aborted")
			return nil
		}
	}
	if err := os.RemoveAll(p.CacheDir); err != nil {
		return err
	}
	fmt.Printf("✓ removed index for %s\n", p.Root)
	return nil
}

// stdinIsTTY reports whether stdin is a character device (terminal).
// Used to gate interactive prompts so scripted invocations don't hang.
func stdinIsTTY() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// ─── reindex ───────────────────────────────────────────────────────────────

func cmdReindex(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("reindex", flag.ContinueOnError)
	setHelp(fs,
		"Drop and re-embed a project from scratch (or every known project with --all --yes).",
		"dex reindex [flags] <path>  |  dex reindex --all --yes")
	all := fs.Bool("all", false, "drop and re-index every known project under DEX_INDEX_DIR")
	yes := fs.Bool("yes", false, "confirm the destructive sweep required by --all")
	verbose := fs.Bool("v", false, "verbose")
	force := fs.Bool("force", false, "bypass protected-path and git-tree guards")
	waitLock := fs.Bool("wait", false, "if another dex indexer is running on this project, wait for it to finish instead of skipping")
	breakLock := fs.Bool("break-lock", false, "discard an existing project lockfile (use only when the prior holder is gone)")
	pullModel := fs.Bool("pull-model", false, "pull the default ollama embedding model (qwen3-embedding:4b) before reindexing")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	base, err := indexDir()
	if err != nil {
		return err
	}
	rest := fs.Args()

	if *pullModel {
		model := embed.DefaultPullModel
		fmt.Printf("pulling ollama model %q …\n", model)
		if err := embed.PullOllamaModel(ctx, model, os.Stdout); err != nil {
			return fmt.Errorf("pull model: %w", err)
		}
		fmt.Printf("pulled %q — continuing with reindex\n", model)
	}

	if *all {
		if len(rest) != 0 {
			return fmt.Errorf("reindex --all takes no path argument")
		}
		if !*yes {
			return fmt.Errorf("reindex --all drops every project index and re-embeds from scratch; pass --yes to confirm")
		}
		roots, err := knownProjectRoots(ctx, base)
		if err != nil {
			return err
		}
		if len(roots) == 0 {
			fmt.Printf("nothing to reindex under %s\n", base)
			return nil
		}
		var failed []string
		for _, root := range roots {
			fmt.Printf("→ reindexing %s\n", root)
			if err := reindexOne(ctx, root, base, *verbose, *force, *waitLock, *breakLock); err != nil {
				fmt.Fprintf(os.Stderr, "  ✗ %v\n", err)
				failed = append(failed, root)
			}
		}
		if len(failed) > 0 {
			return fmt.Errorf("%d of %d project(s) failed to reindex", len(failed), len(roots))
		}
		return nil
	}

	if len(rest) != 1 {
		return fmt.Errorf("reindex needs exactly one path argument (or --all)")
	}
	return reindexOne(ctx, rest[0], base, *verbose, *force, *waitLock, *breakLock)
}

// reindexOne drops the existing per-project cache dir and re-runs the
// indexer from scratch. Used by both `reindex <path>` and the loop in
// `reindex --all`.
func reindexOne(ctx context.Context, root, base string, verbose, force, waitLock, breakLock bool) error {
	p, err := proj.Resolve(root, base)
	if err != nil {
		return err
	}
	if err := proj.CheckIndexable(p, force); err != nil {
		return err
	}
	// Ensure the cache dir exists before acquiring the lock — the lock
	// file lives inside it. The destructive sweep below preserves the
	// lockfile so the holder fd stays valid.
	if err := p.EnsureCacheDir(); err != nil {
		return err
	}
	lk, err := acquireProjectLock(ctx, p, "reindex", "chunk", waitLock, breakLock)
	if err != nil {
		return err
	}
	if lk == nil {
		return nil // another indexer is running; message already printed
	}
	defer func() { _ = lk.Release() }()
	// Read the embed model recorded in the existing index before clearing it.
	// Preserved as the default so a plain `dex reindex` (no DEX_EMBED_MODEL)
	// stays consistent with the original build and won't produce a dim mismatch.
	var priorEmbedModel string
	if prior, err := openStore(ctx, p.DBPath); err == nil {
		priorEmbedModel = prior.EmbedModel()
		_ = prior.Close()
	}
	if err := clearCacheKeepLock(p); err != nil {
		return err
	}
	st, err := openStore(ctx, p.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()
	ig, err := ignore.New(p.Root)
	if err != nil {
		return err
	}
	warnIfNoInclude(ig, p.Root)
	ixOpts := index.Options{Verbose: verbose, Logger: cliLogger(), Concurrency: envInt("DEX_INDEX_CONCURRENCY", 0)}
	ix := index.New(p, st, newEmbedClient(priorEmbedModel), ig, ixOpts)
	if err := ix.Run(ctx); err != nil {
		return err
	}
	if err := st.SetProjectRoot(ctx, p.Root); err != nil {
		return err
	}
	_ = lk.SetPhase("graph")
	gstats, gerr := runGraphPhase(ctx, p, st, verbose)
	if gerr != nil {
		fmt.Fprintf(os.Stderr, "⚠ graph phase failed for %s: %v (chunk index is still usable)\n", p.Root, gerr)
	}
	stats, err := st.Stats(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "✓ reindexed %s\n", p.Root)
	fmt.Fprintf(os.Stderr, "  chunks: %d  files: %d  dim: %d\n", stats.Chunks, stats.Files, stats.Dim)
	if gstats != nil {
		_ = reportGraphStats(p.Root, gstats, "text")
	}
	if gstats != nil {
		if em := newEmbedClient(st.EmbedModel()); em != nil {
			if _, err := embedGraphNodes(ctx, st, em, false); err != nil {
				fmt.Fprintf(os.Stderr, "⚠ graph-embed failed for %s: %v\n", p.Root, err)
			}
		}
	}
	return nil
}

// knownProjectRoots walks the index dir, opening each per-project index
// and reading the recorded `project_root` meta. Entries written before
// that meta existed are reported to stderr and skipped — the user can
// `dex nuke <path>` + `dex index <path>` once to re-record it.
func knownProjectRoots(ctx context.Context, base string) ([]string, error) {
	entries, err := os.ReadDir(base)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var roots []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dbPath := filepath.Join(base, e.Name(), "index.db")
		if _, err := os.Stat(dbPath); err != nil {
			continue
		}
		st, err := openStore(ctx, dbPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: skipping %s: open: %v\n", e.Name(), err)
			continue
		}
		root, err := st.ProjectRoot(ctx)
		st.Close()
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: skipping %s: %v\n", e.Name(), err)
			continue
		}
		if root == "" {
			fmt.Fprintf(os.Stderr, "warning: skipping %s: no recorded project_root (pre-migration index)\n", e.Name())
			continue
		}
		roots = append(roots, root)
	}
	return roots, nil
}

// ─── watch ─────────────────────────────────────────────────────────────────

// envBool parses an env var as a boolean. Truthy: 1, on, true, yes
// (case-insensitive). Falsy: 0, off, false, no. Anything else (or
// unset) returns def.
func envBool(name string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "on", "true", "yes":
		return true
	case "0", "off", "false", "no":
		return false
	default:
		return def
	}
}

// envDuration reads a duration env var. Falls back to def with a
// warning on a parse error; honours def when unset.
func envDuration(name string, def time.Duration) time.Duration {
	raw := os.Getenv(name)
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %s=%q is not a duration; using %s\n", name, raw, def)
		return def
	}
	return d
}

func cmdWatch(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	setHelp(fs,
		"Keep the index fresh as files change (foreground; runs chunk + graph after each debounce).",
		"dex watch [flags] <path>")
	verbose := fs.Bool("v", false, "verbose")
	force := fs.Bool("force", false, "bypass protected-path and git-tree guards")
	debounce := fs.Duration("debounce", 500*time.Millisecond, "quiet window before re-indexing")
	waitLock := fs.Bool("wait", false, "if another dex indexer is running on this project, wait for it to finish instead of skipping")
	breakLock := fs.Bool("break-lock", false, "discard an existing project lockfile (use only when the prior holder is gone)")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return fmt.Errorf("watch needs exactly one path argument")
	}
	base, err := indexDir()
	if err != nil {
		return err
	}
	p, err := proj.Resolve(rest[0], base)
	if err != nil {
		return err
	}
	if err := proj.CheckIndexable(p, *force); err != nil {
		return err
	}
	if err := p.EnsureCacheDir(); err != nil {
		return err
	}
	lk, err := acquireProjectLock(ctx, p, "watch", "chunk", *waitLock, *breakLock)
	if err != nil {
		return err
	}
	if lk == nil {
		return nil // another indexer is running; message already printed
	}
	defer func() { _ = lk.Release() }()
	st, err := openStore(ctx, p.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()
	ig, err := ignore.New(p.Root)
	if err != nil {
		return err
	}
	warnIfNoInclude(ig, p.Root)
	logger := cliLogger()

	ixOpts := index.Options{
		Verbose:     *verbose,
		Logger:      logger,
		Concurrency: envInt("DEX_INDEX_CONCURRENCY", 0),
	}
	ix := index.New(p, st, newEmbedClient(st.EmbedModel()), ig, ixOpts)

	// Refresh the Go static graph after each chunk-index flush. The
	// graph layer lives in the same SQLite file, so the chunk run has
	// already released the writer when this fires.
	afterIndex := func(c context.Context) error {
		if _, err := runGraphPhase(c, p, st, *verbose); err != nil {
			return err
		}
		if em := newEmbedClient(st.EmbedModel()); em != nil {
			if _, err := embedGraphNodes(c, st, em, false); err != nil {
				fmt.Fprintf(os.Stderr, "⚠ graph-embed failed: %v\n", err)
			}
		}
		return nil
	}
	wOpts := watch.Options{
		Debounce:   *debounce,
		Verbose:    *verbose,
		Logger:     logger,
		AfterIndex: afterIndex,
	}
	w := watch.New(ix, ig, p.Root, wOpts)
	return w.Run(ctx)
}

// ─── clone ─────────────────────────────────────────────────────────────────

// cmdClone seeds dst's per-project cache from src's. Useful when the same
// repository is checked out in multiple locations (e.g. git worktrees,
// branch-per-folder workflows). Chunks are keyed by (relative path,
// content sha1), so the copied index is correct for any file that exists
// at the same path with the same content in dst; differing files get
// reconciled on the next `dex index <dst>` (incremental — only
// changed chunks are re-embedded).
func cmdClone(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("clone", flag.ContinueOnError)
	setHelp(fs,
		"Seed dst's index from src's (e.g. for a new git worktree). Follow with `dex index <dst>` to reconcile.",
		"dex clone [flags] <src-path> <dst-path>")
	force := fs.Bool("force", false, "overwrite dst's index if it already exists")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 2 {
		return fmt.Errorf("clone needs <src-path> <dst-path>")
	}
	base, err := indexDir()
	if err != nil {
		return err
	}
	src, err := proj.Resolve(rest[0], base)
	if err != nil {
		return fmt.Errorf("resolve src: %w", err)
	}
	dst, err := proj.Resolve(rest[1], base)
	if err != nil {
		return fmt.Errorf("resolve dst: %w", err)
	}
	if src.ID == dst.ID {
		return fmt.Errorf("src and dst resolve to the same project root (%s); nothing to clone", src.Root)
	}
	if _, err := os.Stat(src.DBPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("src has no index at %s — run `dex index %s` first", src.DBPath, src.Root)
		}
		return err
	}
	if _, err := os.Stat(dst.DBPath); err == nil {
		if !*force {
			return fmt.Errorf("dst already has an index at %s — pass --force to overwrite or `dex nuke %s` first", dst.DBPath, dst.Root)
		}
		if err := os.RemoveAll(dst.CacheDir); err != nil {
			return fmt.Errorf("remove existing dst cache: %w", err)
		}
	}
	if err := dst.EnsureCacheDir(); err != nil {
		return err
	}
	// Copy index.db. SQLite WAL files are not copied — they're either
	// already checkpointed (idle index) or will be rebuilt on next open.
	if err := copyFile(src.DBPath, dst.DBPath); err != nil {
		return fmt.Errorf("copy index: %w", err)
	}
	// Re-tag project_root so `reindex --all` / status see this cache
	// as belonging to dst, not src. A subsequent `dex index <dst>`
	// would also do this, but tagging now keeps the cache discoverable
	// even before the first reconcile.
	if err := retagProjectRoot(ctx, dst.DBPath, dst.Root); err != nil {
		return fmt.Errorf("retag project root: %w", err)
	}
	fmt.Printf("✓ cloned %s → %s\n", src.Root, dst.Root)
	fmt.Printf("  next: `dex index %s` will reconcile any files that differ between the two trees (incremental — only changed chunks are re-embedded).\n", dst.Root)
	return nil
}

// retagProjectRoot opens the cloned DB just long enough to overwrite
// the project_root meta key, so the dst cache no longer claims to be
// src's index.
func retagProjectRoot(ctx context.Context, dbPath, root string) error {
	st, err := openStore(ctx, dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	return st.SetProjectRoot(ctx, root)
}

func copyFile(srcPath, dstPath string) error {
	// Hard-link is instant when src and dst are on the same filesystem.
	if err := os.Link(srcPath, dstPath); err == nil {
		return nil
	}
	in, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// ─── mcp ───────────────────────────────────────────────────────────────────

func cmdMCP(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	remote := fs.String("remote", "", "run as a stdio->REST shim against a remote `dex serve` at this base URL (e.g. http://host:8080); the local index is not used. Bearer token from DEX_SERVE_TOKEN")
	projectID := fs.String("project-id", "", "remote dex project id (sha256 of the canonical host root) to bind tool calls to; with --remote, required unless --project-root is given or the remote serves exactly one project")
	projectRoot := fs.String("project-root", "", "with --remote, compute the project id locally from this path (same-host convenience; the wrong id from a container whose checkout path differs from the host root — use --project-id there)")
	maintenance := fs.Bool("maintenance", false, "run a stub server that registers all tools but returns an immediate maintenance error on every call; agents fall back to native tools instead of hanging on timeouts")
	reason := fs.String("reason", "", "with --maintenance, a short message explaining why dex is unavailable (e.g. 'upgrading GPU drivers')")
	setHelp(fs,
		"Run dex as an MCP server over stdio (canonical entrypoint for Claude Code).\n"+
			"With --remote, run as a thin shim: speak MCP on stdio and proxy every tool\n"+
			"call to a remote `dex serve` REST endpoint (bearer from DEX_SERVE_TOKEN).\n"+
			"With --maintenance, run a stub that immediately signals agents to use native\n"+
			"tools (Read, Bash/grep) — use this during upgrades or outages.",
		"dex mcp [--remote <url> [--project-id <sha> | --project-root <path>]] [--maintenance [--reason <msg>]]")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("mcp takes no arguments (got %v)", fs.Args())
	}

	if *maintenance {
		if *remote != "" || *projectID != "" || *projectRoot != "" {
			return fmt.Errorf("--maintenance is incompatible with --remote/--project-id/--project-root")
		}
		return mcp.RunStdioMaintenance(ctx, *reason)
	}
	if *reason != "" {
		return fmt.Errorf("--reason is only valid with --maintenance")
	}
	if *remote != "" {
		return runRemoteMCP(ctx, *remote, *projectID, *projectRoot)
	}
	if *projectID != "" || *projectRoot != "" {
		return fmt.Errorf("--project-id/--project-root are only valid with --remote")
	}

	base, err := indexDir()
	if err != nil {
		return err
	}
	srv, _ := newServerFromEnv(base)
	return srv.RunStdio(ctx)
}

// runRemoteMCP launches the `dex mcp --remote` shim: an MCP stdio server
// whose tool calls are proxied to a remote `dex serve` daemon, bound to a
// single project id. The id is resolved in priority order: explicit
// --project-id, then --project-root computed locally (only correct when the
// shim runs on the same host with the same paths), then auto-discovery via
// the daemon's registry when it serves exactly one project.
func runRemoteMCP(ctx context.Context, baseURL, projectID, projectRoot string) error {
	token := os.Getenv("DEX_SERVE_TOKEN")

	id := projectID
	switch {
	case id != "":
		// explicit id — trust it (the only correct option from a container,
		// whose /work realpath differs from the canonical host root).
	case projectRoot != "":
		computed, err := mcp.ProjectID(projectRoot)
		if err != nil {
			return fmt.Errorf("--project-root %q: %w", projectRoot, err)
		}
		id = computed
	default:
		projects, err := mcp.ListRemoteProjects(ctx, baseURL, token, nil)
		if err != nil {
			return fmt.Errorf("resolve project id from %s: %w (pass --project-id explicitly)", baseURL, err)
		}
		switch len(projects) {
		case 0:
			return fmt.Errorf("remote %s serves no projects", baseURL)
		case 1:
			id = projects[0].ID
		default:
			var b strings.Builder
			for _, p := range projects {
				fmt.Fprintf(&b, "\n  %s  %s", p.ID, p.Root)
			}
			return fmt.Errorf("remote %s serves %d projects; pass --project-id <sha>:%s", baseURL, len(projects), b.String())
		}
	}

	return mcp.RunStdioRemote(ctx, mcp.RemoteOptions{
		BaseURL:   baseURL,
		Token:     token,
		ProjectID: id,
	})
}

// newServerFromEnv builds a fully-wired *mcp.Server from the current
// environment. Used by both `cmdMCP` (stdio server) and `cmdContext`
// (one-shot CLI invocation of the context router). The HTTP clients
// are lazy — they don't dial until invoked — so wiring all of them
// is cheap even when the context router only uses Embed.
//
// Returns the shared rerank client as the second value so callers
// that need it for separate purposes (e.g. health reporting) don't
// have to redundantly construct another instance.
func newServerFromEnv(base string) (*mcp.Server, rerank.HealthChecker) {
	var rerankClient rerank.HealthChecker = newRerankClient()
	opts := storeOpts()
	if rerankClient != nil {
		// Wrap in a circuit breaker so a hung rerank backend doesn't
		// drag every search through its full timeout for the next 30s
		// after a string of failures. The same wrapper is shared by
		// status (RerankClient) and search (StoreOpts.Reranker) so the
		// breaker state in `dex index status` reflects what callers
		// actually see.
		rerankClient = rerank.NewBreaker(rerankClient, 3, 30*time.Second)
		opts.Reranker = rerankClient
	}
	chatClient := newChatClient()
	expandClient := newExpandClient(chatClient)
	srv := &mcp.Server{
		EmbedClient:  newEmbedClient(""),
		ChatClient:   chatClient,
		RerankClient: rerankClient,
		ExpandClient: expandClient,
		ExpandMode:   expandDefaultMode(expandClient),
		IndexDir:     base,
		StoreOpts:    opts,
		AutoWatch:    autoWatchConfigFromEnv(),
	}
	return srv, rerankClient
}

// autoWatchConfigFromEnv reads DEX_MCP_AUTOWATCH to build a config for the
// MCP server's lazy per-project watchers. Default: enabled. Each watcher
// refreshes the chunk and graph lanes on change (see runWatcher's
// AfterIndex hook); the debounce window is DEX_WATCH_DEBOUNCE.
func autoWatchConfigFromEnv() mcp.AutoWatchConfig {
	enabled := envBool("DEX_MCP_AUTOWATCH", true)
	if !enabled {
		return mcp.AutoWatchConfig{} // zero value disables
	}
	return mcp.AutoWatchConfig{
		Enabled:  true,
		Debounce: envDuration("DEX_WATCH_DEBOUNCE", 500*time.Millisecond),
		Logger:   cliLogger(),
	}
}
