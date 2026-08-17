// Canonical command registry — the single source of truth for the CLI verb
// surface. Usage text (main_usage.go) and the shell-completion scripts
// (completion.go) are GENERATED from this table, so adding or renaming a
// command happens in exactly one place and the three shells can no longer
// drift out of sync (#469).
//
// The dispatch switch in main.go is kept honest by registry_test.go, which
// asserts every dispatched command has a registry entry and vice versa.
package main

// verbGroup buckets a command for the `dex help` / `dex help all` layout.
type verbGroup int

const (
	groupQuery  verbGroup = iota // the MCP-mirroring query verbs
	groupGraph                   // graph / notes / index-status power lanes
	groupBuild                   // build + maintenance commands
	groupConfig                  // config / setup / introspection
	groupHidden                  // dispatched but not advertised in help
)

// flagSpec describes one flag for completion + usage generation.
type flagSpec struct {
	name    string   // "--intent", "-v"
	desc    string   // one-line description
	arg     bool     // true if the flag takes a value
	choices []string // value-completion set (empty = free-form)
}

// subSpec describes one subcommand of a verb (e.g. `graph callers`).
type subSpec struct {
	name string
	desc string
}

// verbSpec is one top-level command.
type verbSpec struct {
	name     string
	group    verbGroup
	args     string // positional-arg hint, e.g. "[<path>] <q...>"
	summary  string // one-line description (zsh _describe, usage tables)
	subs     []subSpec
	flags    []flagSpec
	noFiles  bool // true = no trailing path/file argument to complete
	fileArgs bool // true = completes plain files (not just dirs) — e.g. read
}

// fmtChoices / fmtFormat reusable flag fragments.
var (
	flagFormat = flagSpec{name: "--format", desc: "output format", arg: true, choices: []string{"text", "json"}}
	flagK      = flagSpec{name: "--k", desc: "max results", arg: true}
	flagV      = flagSpec{name: "-v", desc: "verbose"}
	flagDeep   = flagSpec{name: "--deep", desc: "send one minimal real request per backend (may load models)"}
	flagMaxCB  = flagSpec{name: "--max-content-bytes", desc: "truncation limit in bytes (0=no limit)", arg: true}

	intentChoices = []string{
		"auto", "behavior_search", "symbol_lookup", "callers", "callees",
		"architecture", "package_topology", "editing_context", "assemble",
	}
	// readModeChoices is the canonical CLI `read --mode` set — the single source
	// of truth for usage text, shell completion, and `cmdRead` validation.
	// skeleton/map/summary delegate to Server.Summarize (see serverReadMode);
	// the rest run locally. Parity with the MCP `read` tool's mode set is locked
	// by read_parity_test.go.
	readModeChoices = []string{"full", "signatures", "aggressive", "entropy", "auto", "skeleton", "map", "summary", "analyze"}

	// serverReadModes are the read modes whose logic lives in the index +
	// Server.Summarize handler; cmdRead delegates these instead of computing a
	// view locally, so CLI output matches the MCP `read` tool exactly.
	serverReadModes = map[string]bool{"skeleton": true, "map": true, "summary": true, "analyze": true}
)

// validReadMode reports whether m is an accepted CLI `read --mode`.
func validReadMode(m string) bool {
	for _, c := range readModeChoices {
		if c == m {
			return true
		}
	}
	return false
}

// serverReadMode reports whether mode m is handled by delegating to
// Server.Summarize rather than by a local fast path.
func serverReadMode(m string) bool { return serverReadModes[m] }

// mcpOnlyToolHints maps MCP tools that deliberately have NO CLI verb to a
// help message. The dispatcher consults this on an unknown command so
// `dex <tool>` points at the MCP surface instead of a bare "unknown command"
// (#521). These tools stay off the `verbs` registry by design — the MCP-only
// contract is locked by verb_parity_test.go.
var mcpOnlyToolHints = map[string]string{
	"session":    "session is available via the MCP tool surface, not the CLI — it recaps an active MCP connection's working set (DEX_EXPERT power lane).",
	"checkpoint": "checkpoint is available via the MCP tool surface, not the CLI — it keeps a shadow-git history of an agent's work-in-progress (snapshot/log/diff; DEX_EXPERT power lane).",
}

// mcpOnlyToolHint returns the MCP-only help message for cmd, if any.
func mcpOnlyToolHint(cmd string) (string, bool) {
	h, ok := mcpOnlyToolHints[cmd]
	return h, ok
}

// verbs is the canonical registry. Order within a group is the display order
// in `dex help all`.
var verbs = []verbSpec{
	// ---- query verbs (mirror the MCP tool names, #354/#427) ----
	{
		name: "ask", group: groupQuery, args: "[<path>] <q...>",
		summary: "one-shot router (semantic + symbol + graph)",
		flags: []flagSpec{
			{name: "--intent", desc: "search strategy", arg: true, choices: intentChoices},
			flagK, flagFormat,
			{name: "--no-inline", desc: "skip inlining file contents"},
			flagMaxCB, flagV,
		},
	},
	{
		name: "search", group: groupQuery, args: "[<path>] <q...>",
		summary: "hybrid search — raw ranking (ask composes this)",
		flags: []flagSpec{
			flagK, flagFormat,
			{name: "--rerank", desc: "disable rerank for this query", arg: true, choices: []string{"off"}},
			{name: "--explain", desc: "show per-chunk score breakdown"},
			flagMaxCB, flagV,
		},
	},
	{
		name: "read", group: groupQuery, args: "<file>", fileArgs: true,
		summary: "read a file (--mode full|signatures|summary…)",
		flags: []flagSpec{
			{name: "--mode", desc: "read mode", arg: true, choices: readModeChoices},
			{name: "--start", desc: "first line", arg: true},
			{name: "--end", desc: "last line", arg: true},
			{name: "--focus", desc: "summary steering hint", arg: true},
			{name: "--temperature", desc: "summary sampling temperature", arg: true},
			{name: "--max-tokens", desc: "summary max tokens", arg: true},
			flagFormat, flagV,
		},
	},
	{
		name: "repo_map", group: groupQuery, args: "[--cluster <id>] [<path>]",
		summary: "deterministic repo orientation: first-touch bundle, or --cluster to zoom",
		flags:   []flagSpec{{name: "--cluster", desc: "focus a cluster id", arg: true}, flagFormat, flagV},
	},
	{
		name: "trace", group: groupQuery, args: "[<path>] <name>",
		summary: "call-graph callers/callees/path/impact",
		flags: []flagSpec{
			{name: "--dir", desc: "direction", arg: true, choices: []string{"callers", "callees", "path", "impact"}},
			{name: "--max-depth", desc: "BFS depth (path/impact)", arg: true},
			flagK, flagFormat, flagV,
		},
	},
	{
		name: "locate", group: groupQuery, args: "[<path>] <symbol-or-path:line>",
		summary: "full context for one symbol: callers, tests, doc, blame, notes",
		flags: []flagSpec{
			{name: "--frame", desc: "parse a raw stack-trace frame line", arg: true},
			{name: "--issues", desc: "also list matching open GitHub issues (gh)"},
			flagK, flagFormat,
		},
	},
	{
		name: "cohort", group: groupQuery, args: "[<path>] <interface>",
		summary: "types that must change in lockstep with an interface",
		flags:   []flagSpec{flagFormat},
	},
	{
		name: "refs", group: groupQuery, args: "[<path>] <action> <symbol>",
		summary: "type-precise Go symbol queries — references, implementations, supertypes, subtypes (Go-only)",
		flags: []flagSpec{
			{name: "--action", desc: "references | implementations | supertypes | subtypes (also positional arg 1)", arg: true},
			flagFormat,
		},
	},
	{
		name: "plan_rename", group: groupQuery, args: "[<path>] <symbol> <to>",
		summary: "plan a type-precise rename (edit triples; never writes)",
		flags: []flagSpec{
			{name: "--op", desc: "operation (v1: rename_symbol)", arg: true},
			{name: "--etag", desc: "prior plan etag (detects stale)", arg: true},
			flagFormat,
		},
	},
	{
		name: "rehearse_patch", group: groupQuery, args: "[<path>]",
		summary: "type-check a hypothetical edit in-memory (never writes)",
		flags: []flagSpec{
			{name: "--edits", desc: "JSON array of {path,start_byte,end_byte,replacement} splices", arg: true},
			{name: "--file", desc: "project-relative path for a whole-file replacement", arg: true},
			{name: "--contents", desc: "new file contents for --file", arg: true},
			flagFormat,
		},
	},
	{
		name: "review_diff", group: groupQuery, args: "[<path>]",
		summary: "per-hunk PR intelligence",
		flags: []flagSpec{
			{name: "--ref", desc: "git range or single ref (default HEAD~1..HEAD)", arg: true},
			{name: "--branch", desc: "review what a branch adds vs --base", arg: true},
			{name: "--pr", desc: "GitHub PR number (via gh)", arg: true},
			{name: "--base", desc: "base branch for --branch/--pr (default main)", arg: true},
			{name: "--compact", desc: "drop low-risk hunks"},
			flagK, flagFormat,
		},
	},
	{
		name: "verify_change", group: groupQuery, args: "[<path>]",
		summary: "run the tests a change implicates (working tree / --ref / --symbol)",
		flags: []flagSpec{
			{name: "--ref", desc: "test a git range instead of the working tree", arg: true},
			{name: "--symbol", desc: "test a symbol's blast-radius instead of a diff", arg: true},
			{name: "--command", desc: "override test command template ({{packages}})", arg: true},
			{name: "--timeout", desc: "test-run timeout seconds", arg: true},
			flagFormat,
		},
	},
	{
		name: "check", group: groupQuery, args: "[<path>] <ref...>",
		summary: "verify file:line[:symbol] references against the index",
		flags: []flagSpec{
			flagFormat,
		},
	},
	{
		name: "grep", group: groupQuery, args: "[<path>] <pattern>",
		summary: "exact RE2 regex search",
		flags: []flagSpec{
			{name: "--ext", desc: "file extension filter (no dot)", arg: true},
			{name: "--in", desc: "restrict to a subdirectory", arg: true},
			{name: "--max-results", desc: "maximum matches", arg: true},
			flagFormat,
		},
	},
	{
		name: "shell", group: groupHidden, args: "<command...>",
		summary: "run a command with compressed output",
		flags: []flagSpec{
			{name: "--cwd", desc: "working directory", arg: true},
			{name: "--raw", desc: "skip compression"},
			flagFormat,
		},
	},

	// ---- graph / notes / power lanes ----
	{
		name: "graph", group: groupGraph, args: "<sub> [<path>] …",
		summary: "graph traversal",
		subs: []subSpec{
			{"neighbors", "vector neighbours of a chunk"},
			{"similar", "blocks semantically near a given block"},
			{"clones", "clusters of near-duplicate code blocks"},
			{"deps", "imports edges for a file or package"},
			{"packages", "whole internal package import DAG"},
			{"links", "markdown docs this doc links to"},
			{"backlinks", "markdown docs that link to this doc"},
			{"tags", "tag→docs or doc→tags"},
			{"cycles", "import cycles"},
			{"diff", "blast-radius of a git diff"},
			{"clusters", "Louvain call-graph clusters"},
			{"smells", "long funcs, dead exports, god files/nodes"},
			{"routes", "HTTP/MCP/gRPC handlers + registration sites"},
			{"export", "dump nodes/edges as JSONL"},
		},
		flags: []flagSpec{flagK, flagFormat, flagV},
	},
	{
		name: "notes", group: groupGraph, args: "add|list|delete|review|pin|unpin|gc|relate|relations",
		summary: "per-project notes",
		subs: []subSpec{
			{"add", "store a fact"},
			{"list", "top-k facts by salience"},
			{"delete", "delete a fact by id"},
			{"gc", "decay + consolidate + evict"},
			{"relate", "create/reinforce a typed edge between facts"},
			{"relations", "list edges for a fact, or full Mermaid diagram"},
		},
		flags: []flagSpec{flagFormat},
	},

	// ---- build / maintenance ----
	{
		name: "index", group: groupBuild, args: "<path>",
		summary: "build or refresh the project index",
		subs:    []subSpec{{"status", "endpoint health and project stats"}},
		flags: []flagSpec{
			{name: "--graph", desc: "graph phase mode", arg: true, choices: []string{"on", "off", "only"}},
			flagFormat,
			{name: "--dry-run", desc: "preview without writing"},
			flagV,
			{name: "--force", desc: "bypass guards"},
			{name: "--wait", desc: "wait for lock"},
		},
	},
	{
		name: "status", group: groupBuild, args: "[<path>]", fileArgs: true,
		summary: "endpoint health + project stats (alias: index status)",
		flags:   []flagSpec{flagFormat, flagV},
	},
	{
		name: "compact", group: groupBuild, args: "<path>",
		summary: "dump all indexable files to stdout (LLM context prep)",
		flags: []flagSpec{
			{name: "--out", desc: "write to FILE", arg: true},
			{name: "--max-bytes", desc: "byte budget", arg: true},
			{name: "--strip", desc: "strip comments/blank lines"},
		},
	},
	{
		name: "compress", group: groupBuild, args: "<file|->",
		summary: "compress a file or stdin through the dex engine (no LLM)",
		flags: []flagSpec{
			{name: "--mode", desc: "compression mode", arg: true, choices: []string{"auto", "aggressive", "entropy", "terse", "off"}},
			{name: "--ext", desc: "language hint", arg: true},
			{name: "--out", desc: "write to FILE", arg: true},
			flagFormat,
		},
	},
	{
		name: "nuke", group: groupBuild, args: "<path>",
		summary: "delete the on-disk index for a project",
		flags:   []flagSpec{{name: "--yes", desc: "skip confirmation"}},
	},
	{
		name: "reindex", group: groupBuild, args: "<path>|--all",
		summary: "drop and re-embed from scratch",
		flags:   []flagSpec{{name: "--all", desc: "every known project"}, {name: "--yes", desc: "skip confirmation"}},
	},
	{
		name: "summarize", group: groupBuild, args: "[<path>...]", fileArgs: true,
		summary: "generate per-file LLM summaries into the index (isolated from search)",
		flags: []flagSpec{
			{name: "--get", desc: "print a stored summary as JSON, without generating", arg: true},
			{name: "--force", desc: "regenerate even when the source hash is unchanged"},
			{name: "--focus", desc: "optional focus hint for the summarizer", arg: true},
			flagFormat,
			flagV,
		},
	},
	{
		name: "watch", group: groupBuild, args: "<path>",
		summary: "keep the index fresh as files change",
		flags: []flagSpec{
			flagV,
			{name: "--debounce", desc: "quiet window before re-indexing", arg: true},
			{name: "--force", desc: "bypass guards"},
		},
	},
	{
		name: "clone", group: groupBuild, args: "<src> <dst>",
		summary: "seed dst index from src (worktrees)",
	},
	{
		name: "bench", group: groupBuild, args: "<sub> [<path>]",
		summary: "benchmarks: eval|corpus|compress|perf|locomo",
		subs: []subSpec{
			{"eval", "retrieval eval against the golden set"},
			{"corpus", "multi-repo retrieval eval"},
			{"compress", "compression ratio/quality"},
			{"perf", "indexing + query latency"},
			{"locomo", "LoCoMo memory-recall benchmark"},
		},
	},
	{
		name: "feedback", group: groupBuild, summary: "relevance of ask suggested_reads on real traffic (reads hooks.jsonl)", noFiles: true,
		flags: []flagSpec{
			{name: "--json", desc: "emit the report as JSON"},
			{name: "--window", desc: "consume-event lookahead per suggested read (0 = whole session)", arg: true},
			{name: "--log", desc: "path to hooks.jsonl", arg: true},
		},
	},
	{
		name: "hook", group: groupBuild, args: "inject|rewrite|redirect|observe",
		summary: "Claude Code hook scripts",
		subs: []subSpec{
			{"inject", "UserPromptSubmit hook — inject dex context"},
			{"rewrite", "Bash hook — rewrite rg/grep to dex find"},
			{"redirect", "Read/Grep hook — compress large files"},
			{"observe", "PostToolUse/Stop hook — append event log"},
		},
	},

	// ---- config / setup / runtime ----
	{
		name: "mcp", group: groupConfig, summary: "run as an MCP server over stdio", noFiles: true,
	},
	{
		name: "serve", group: groupConfig, args: "[flags] --project <p>",
		summary: "run as an HTTP daemon (multi-project)",
		flags: []flagSpec{
			{name: "--addr", desc: "listen address", arg: true},
			{name: "--project", desc: "project root (repeatable)", arg: true},
		},
	},
	{
		name: "proxy", group: groupConfig, args: "<path>",
		summary: "MCP proxy — forward tools to a remote dex server",
	},
	{
		name: "setup", group: groupConfig, summary: "guided first-run wizard", noFiles: true,
		flags: []flagSpec{{name: "--check", desc: "exit 0 if setup complete, 1 otherwise"}},
	},
	{
		name: "doctor", group: groupConfig, summary: "check the dex setup", noFiles: true,
		flags: []flagSpec{flagV, flagDeep},
	},
	{
		name: "config", group: groupConfig, args: "init",
		summary: "manage .dex/config.yml",
		subs:    []subSpec{{"init", "scaffold .dex/config.yml with commented defaults"}},
		flags: []flagSpec{
			{name: "--force", desc: "overwrite existing file"},
			{name: "--full", desc: "include all tuning knobs"},
		},
	},
	{
		name: "env", group: groupConfig, summary: "print effective DEX_* configuration", noFiles: true,
		flags: []flagSpec{
			{name: "--all", desc: "include tuning knobs"},
			{name: "--doc", desc: "include documentation"},
			flagV, flagFormat,
		},
	},
	{
		name: "completion", group: groupConfig, args: "bash|zsh|fish",
		summary: "generate tab-completion script (bash|zsh|fish)",
		subs:    []subSpec{{"bash", ""}, {"zsh", ""}, {"fish", ""}},
	},
	{
		name: "version", group: groupConfig, summary: "print the build version", noFiles: true,
	},

	// ---- hidden (dispatched, not advertised) ----
	{name: "agent", group: groupHidden, args: "announce|post|read|list", summary: "multi-agent coordination bus (swarm findings)"},
	{name: "compress-stdin", group: groupHidden, summary: "compress stdin through dex patterns", noFiles: true},
	{name: "shell-hook", group: groupHidden, summary: "print eval-able shell hook for passive compression", noFiles: true},
}

// completionCommands returns the advertised command names (canonical name
// only, no aliases, excluding hidden plumbing) in registry order — the single
// source for every shell's top-level command list.
func completionCommands() []string {
	out := make([]string, 0, len(verbs))
	for _, v := range verbs {
		if v.group == groupHidden {
			continue
		}
		out = append(out, v.name)
	}
	return out
}

// zshCommandList renders the advertised verbs as zsh `_describe` entries
// ('name:summary'), registry order.
func zshCommandList() []string {
	out := make([]string, 0, len(verbs))
	for _, v := range verbs {
		if v.group == groupHidden {
			continue
		}
		out = append(out, v.name+":"+v.summary)
	}
	return out
}
