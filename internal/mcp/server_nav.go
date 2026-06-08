package mcp

import (
	"context"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// NavInput carries no parameters — ctx_nav is a zero-argument orientation call.
type NavInput struct{}

// NavEntry describes one dex tool for programmatic consumption.
type NavEntry struct {
	Name    string `json:"name"`
	Tier    string `json:"tier"`    // "all" | "standard" | "power"
	Purpose string `json:"purpose"` // one-line description of what the tool does
	When    string `json:"when"`    // one-line guidance on when to call it
}

// NavOutput is the response from ctx_nav.
type NavOutput struct {
	Status string     `json:"status"`
	Guide  string     `json:"guide"` // concise markdown routing guide
	Tools  []NavEntry `json:"tools"` // structured tool list for the active tier
}

// allNavEntries is the full catalogue ordered from most-used to least.
// Tier: "all" = TierAsk+, "standard" = TierStandard+, "power" = TierPower only.
var allNavEntries = []NavEntry{
	{
		Name:    "ask",
		Tier:    "all",
		Purpose: "Primary code-understanding entry point — composes semantic search, symbol lookup, graph expansion, and (optionally) a synthesized prose answer in one call.",
		When:    "Before any Grep/Read/Glob fan-out on a code question: where, how, callers, architecture, editing context. Follow its next_action; honor its avoid.",
	},
	{
		Name:    "ctx_nav",
		Tier:    "standard",
		Purpose: "Return this tool-routing guide — which tools exist, their tier, and when to call each.",
		When:    "Once at session start in an unfamiliar project, or whenever you need a reminder of the dex tool surface.",
	},
	{
		Name:    "ctx_overview",
		Tier:    "standard",
		Purpose: "Task-relevant project map — ranks every indexed file by semantic similarity to the task fused with graph centrality.",
		When:    "First call in an unfamiliar codebase to decide what to read before touching code. Returns file paths only (cheaper than ask).",
	},
	{
		Name:    "ctx_knowledge",
		Tier:    "standard",
		Purpose: "Persistent project knowledge store — facts, patterns, gotchas that survive session resets.",
		When:    "add: record an architectural decision or gotcha. list: retrieve high-salience facts before starting a task. High-salience facts auto-inject into ask responses.",
	},
	{
		Name:    "ctx_session",
		Tier:    "standard",
		Purpose: "Per-project session memory — task declaration, findings, files read/written.",
		When:    "set_task at session start; add_note when you find something important; get to recover state after a reconnect.",
	},
	{
		Name:    "ctx_agent",
		Tier:    "standard",
		Purpose: "Multi-agent coordination bus — share findings across concurrent agents on the same dex instance.",
		When:    "announce at startup in multi-agent runs; post findings; read peers' messages before duplicating work.",
	},
	{
		Name:    "ctx_shell",
		Tier:    "standard",
		Purpose: "Execute a shell command and return compressed output (strips build noise, collapses log lines).",
		When:    "When you need to run a build, test, or git command and don't want raw output burning context budget.",
	},
	{
		Name:    "file_tree",
		Tier:    "standard",
		Purpose: "List indexed files under a directory path with chunk counts.",
		When:    "Orientation in an unfamiliar subtree. No embedding required — instant from the index.",
	},
	{
		Name:    "search_grep",
		Tier:    "standard",
		Purpose: "Regex search over project files — no embedding required.",
		When:    "Exact-match queries: cross-cutting symbol references, import paths, string literals. Use ask for conceptual queries.",
	},
	{
		Name:    "search_context",
		Tier:    "standard",
		Purpose: "Single-call: embed query → top-k relevant files → symbol signatures + best-match body.",
		When:    "Task-start orientation when you want file + symbol context in one round trip without ask's full routing overhead.",
	},
	{
		Name:    "file_view",
		Tier:    "standard",
		Purpose: "Send a file slice to the chat model for a digested summary (signatures, structure, gist).",
		When:    "You already know which file to digest. Prefer ask's suggested_reads first — they name the right file.",
	},
	{
		Name:    "ctx_prefetch",
		Tier:    "standard",
		Purpose: "Compute blast-radius of changed files and read the most relevant graph neighbors.",
		When:    "After editing a file, to proactively read what it affects — surfaces implementors, callers, and test files before you ask for them. Requires graph index.",
	},
	// Power-tier tools (shown only in TierPower responses)
	{
		Name:    "search_semantic",
		Tier:    "power",
		Purpose: "Raw vector search — embeds query, returns top-k matching chunks with RRF fusion.",
		When:    "You specifically want raw ranking without intent routing. Prefer ask.",
	},
	{
		Name:    "search_symbol",
		Tier:    "power",
		Purpose: "Fast SQL exact-identifier lookup — no embedding.",
		When:    "You have the exact identifier and want nothing else. ask runs this automatically.",
	},
	{
		Name:    "graph_neighbors",
		Tier:    "power",
		Purpose: "Cosine neighbors of a known chunk by (path, start_line).",
		When:    "You have an exact chunk location and want semantically related code. ask includes this.",
	},
	{
		Name:    "graph_deps",
		Tier:    "power",
		Purpose: "Import edges for a file or package from the static graph.",
		When:    "Direct dependency inspection without ask overhead.",
	},
	{
		Name:    "graph_callers",
		Tier:    "power",
		Purpose: "Functions that CALL the given symbol (Go call graph).",
		When:    "Direct call-chain inspection. ask uses callers intent automatically.",
	},
	{
		Name:    "graph_callees",
		Tier:    "power",
		Purpose: "Functions that the given symbol CALLS (Go call graph).",
		When:    "Direct call-chain inspection. ask uses callees intent automatically.",
	},
	{
		Name:    "graph_impact",
		Tier:    "power",
		Purpose: "Transitive blast-radius analysis — follows calls edges outward up to max_depth.",
		When:    "Before editing a widely-called symbol to gauge ripple. Use ask with callers intent first.",
	},
	{
		Name:    "graph_links",
		Tier:    "power",
		Purpose: "Outgoing link/wikilink edges from a markdown doc.",
		When:    "Navigating a doc graph by outgoing links.",
	},
	{
		Name:    "graph_backlinks",
		Tier:    "power",
		Purpose: "Incoming link/wikilink edges to a markdown doc.",
		When:    "Finding what references a spec or note.",
	},
	{
		Name:    "graph_tags",
		Tier:    "power",
		Purpose: "Query the markdown tag graph — list docs by tag or tags by doc.",
		When:    "Tag-based doc clustering.",
	},
	{
		Name:    "graph_routes",
		Tier:    "power",
		Purpose: "Detect HTTP handlers, MCP tool registrations, and gRPC service implementations from the call graph.",
		When:    "Understanding a service's public surface before reading its handlers.",
	},
	{
		Name:    "graph_smells",
		Tier:    "power",
		Purpose: "AST-based code quality signals: long functions, dead exports, god files.",
		When:    "Before a PR or refactor to spot obvious structural issues.",
	},
	{
		Name:    "compress_output",
		Tier:    "power",
		Purpose: "Compress raw shell output — strips build noise, go test failures only, git diff context lines.",
		When:    "When you ran a command yourself and want to compress before storing in context. Use ctx_shell to run + compress in one step.",
	},
	{
		Name:    "spec_check",
		Tier:    "power",
		Purpose: "Verify a spec file's checklist items against the code index with LLM judgment.",
		When:    "Drift detection between a spec and the implementation.",
	},
	{
		Name:    "status",
		Tier:    "power",
		Purpose: "Report dex endpoint health and indexed projects with chunk counts.",
		When:    "Debugging connectivity or confirming a project is indexed.",
	},
}

const navGuide = `# dex tool-routing guide

## Rule #1 — ask first

Before any Grep/Glob/Read fan-out on a code-understanding question, call **ask**.
It picks intent automatically, composes semantic search + symbol lookup + graph
expansion, and returns suggested_reads, a prose next_action directive, and an
avoid line. Execute next_action verbatim; honor avoid.

## When NOT to call ask

- You have an exact file path and just need to read it → **Read** (native).
- You're hunting an exact literal (error message, magic number) → **Grep** or **search_grep**.
- You're editing → **Edit** (native).
- You need to orient in an unfamiliar project before your first question → **ctx_overview**.

## Standard tool routing

| Question type | Tool |
|---|---|
| Where / how / callers / architecture / editing context | **ask** |
| What files are relevant to this task? | **ctx_overview** |
| What files are in this directory? | **file_tree** |
| Exact regex match across the codebase | **search_grep** |
| Digest a known file (signatures, structure) | **file_view** |
| Quick context: relevant files + best symbol body | **search_context** |
| Store a fact / gotcha for future sessions | **ctx_knowledge** add |
| Remember what you're working on | **ctx_session** set_task |
| Run a command without burning context | **ctx_shell** |

## Fallback behaviors

- ask returns **status: "no-index"** → run ` + "`dex index <project>`" + ` once, or fall back to Grep.
- ask returns **status: "embedding-service-unreachable"** → embed is offline; use Grep.
- ask returns **stale: true** → results may be behind HEAD; flag this if the fix depends on recent code.
`

func (s *Server) nav(_ context.Context, _ *sdk.CallToolRequest, _ NavInput) (*sdk.CallToolResult, NavOutput, error) {
	// toolTierFromEnv is the authoritative source — the tier is configured via
	// DEX_TOOLS at startup and doesn't change during a server's lifetime.
	entries := navEntriesForTier(toolTierFromEnv())
	out := NavOutput{
		Status: "ok",
		Guide:  navGuide,
		Tools:  entries,
	}
	// Return the guide as readable markdown in Content; structured tool list is
	// also available via StructuredContent (the SDK fills that from out).
	res := &sdk.CallToolResult{
		Content: []sdk.Content{&sdk.TextContent{Text: navText(out)}},
	}
	return res, out, nil
}

// navEntriesForTier filters allNavEntries to those visible at tier t.
func navEntriesForTier(t toolTier) []NavEntry {
	var out []NavEntry
	for _, e := range allNavEntries {
		switch e.Tier {
		case "all":
			out = append(out, e)
		case "standard":
			if t >= TierStandard {
				out = append(out, e)
			}
		case "power":
			if t >= TierPower {
				out = append(out, e)
			}
		}
	}
	return out
}

func navText(out NavOutput) string {
	var b strings.Builder
	b.WriteString(out.Guide)
	b.WriteString("\n## Available tools\n\n")
	for _, e := range out.Tools {
		b.WriteString("### ")
		b.WriteString(e.Name)
		b.WriteString(" (")
		b.WriteString(e.Tier)
		b.WriteString(")\n")
		b.WriteString(e.Purpose)
		b.WriteString("\n*When:* ")
		b.WriteString(e.When)
		b.WriteString("\n\n")
	}
	return b.String()
}
