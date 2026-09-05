// Canonical command registry — the single source of truth for the CLI verb
// surface. The shell-completion scripts (completion.go) are GENERATED from
// this table, so adding or renaming a command keeps the three shells from
// drifting out of sync (#469). The `dex help` / `dex help all` screens
// (main_usage.go) are hand-authored prose, NOT generated from this table —
// keep a new/renamed/re-annotated command's registry entry and its
// main_usage.go line in sync by hand.
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
	flagK      = flagSpec{name: "--k", desc: "max results per lane", arg: true}
	flagV      = flagSpec{name: "-v", desc: "verbose"}
	flagDeep   = flagSpec{name: "--deep", desc: "send one minimal real request per backend (may load models)"}

	// queryKindChoices is query's kind= ladder — the single source of truth for
	// usage text and shell completion (#849).
	queryKindChoices = []string{
		"read", "grep", "locate", "symbol", "callers", "callees", "impact", "path",
		"search", "editing", "assemble", "architecture", "packages", "orient", "review",
		"check", "refs", "cohort", "deps", "status",
	}
)

// mcpOnlyToolHints maps a command name to a help message the dispatcher shows
// on an unknown command, instead of a bare "unknown command" (#521). Two
// disjoint reasons a name lands here: an MCP tool that deliberately has NO CLI
// verb at all (these stay off the `verbs` registry by design — the MCP-only
// contract is locked by verb_parity_test.go), or a verb the CLI collapse
// (#849) deleted outright with no alias — same "point at the real door,
// don't just say unknown" reasoning, redirecting muscle memory to
// `dex query --kind=…` instead of silently keeping the old name alive.
var mcpOnlyToolHints = map[string]string{
	"ask":         "`dex ask` was folded into `dex query` (#849) — try `dex query <input>` (prose routes to the same search+synthesis lanes; --kind=assemble for a task working set)",
	"search":      "`dex search` was folded into `dex query --kind=search <q>` (#849)",
	"read":        "`dex read` was folded into `dex query --kind=read <file>` (#849) — default is compressed signatures; --want=full for raw content",
	"locate":      "`dex locate` was folded into `dex query --kind=locate <symbol-or-path:line>` (#849)",
	"trace":       "`dex trace` was folded into `dex query --kind=callers|callees|impact|path <name>` (#849)",
	"grep":        "`dex grep` was folded into `dex query --kind=grep <pattern>` (#849)",
	"review_diff": "`dex review_diff` was folded into `dex query --kind=review` for the working-tree case (#849); a targeted --ref/--branch/--pr review has no CLI front door anymore (MCP review_diff tool, DEX_EXPERT, still has it)",
	"repo_map":    "`dex repo_map` was folded into `dex query --kind=orient` (#849) for the default first-touch bundle; --cluster/--around/--around-diff zooms have no CLI front door anymore",
	"check":       "`dex check` was folded into `dex query --kind=check <ref...>` (#849)",
	"refs":        "`dex refs` was folded into `dex query --kind=refs --want=<action> <symbol>` (#849)",
	"cohort":      "`dex cohort` was folded into `dex query --kind=cohort <interface>` (#849)",
}

// mcpOnlyToolHint returns the MCP-only help message for cmd, if any.
func mcpOnlyToolHint(cmd string) (string, bool) {
	h, ok := mcpOnlyToolHints[cmd]
	return h, ok
}

// verbs is the canonical registry. Order within a group is the display order
// in `dex help all`.
var verbs = []verbSpec{
	// ---- query verb (the single read verb, #849 — folds in the former
	// ask/search/read/repo_map/trace/locate/cohort/refs/check/grep/review_diff/
	// graph-deps/status verbs; see specs/query-unification.md) ----
	{
		name: "query", group: groupQuery, args: "[<path>] <input...>",
		summary: "the read verb — one call to read the codebase intelligence",
		flags: []flagSpec{
			{name: "--kind", desc: "force the lane", arg: true, choices: queryKindChoices},
			{name: "--want", desc: "facet within the lane (e.g. signatures|map|skeleton|answer|assemble|callers|callees|impact|path|references|implementations|supertypes|subtypes)", arg: true},
			{name: "--to", desc: "destination symbol for the graph 'path' facet", arg: true},
			{name: "--budget", desc: "context-token budget", arg: true},
			{name: "--project-root", desc: "explicit absolute project/worktree root", arg: true},
			flagK,
			{name: "--context", desc: "grep lane: lines of context per match", arg: true},
			{name: "--fixed", desc: "grep lane: literal match, not regex"},
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
	// ---- graph / notes / power lanes ----
	{
		name: "graph", group: groupGraph, args: "<sub> [<path>] …",
		summary: "graph traversal",
		subs: []subSpec{
			{"neighbors", "vector neighbours of a chunk"},
			{"similar", "blocks semantically near a given block"},
			{"clones", "clusters of near-duplicate code blocks"},
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
		name: "hook", group: groupBuild, args: "inject|redirect|observe",
		summary: "Claude Code hook scripts",
		subs: []subSpec{
			{"inject", "UserPromptSubmit hook — inject dex context"},
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
