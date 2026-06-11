package mcp

import (
	"context"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// NavInput carries no parameters — nav is a zero-argument orientation call.
type NavInput struct{}

// NavEntry describes one dex tool for programmatic consumption.
type NavEntry struct {
	Name    string `json:"name"`
	Purpose string `json:"purpose"` // one-line description of what the tool does
	When    string `json:"when"`    // one-line guidance on when to call it
}

// NavOutput is the response from the nav handler.
type NavOutput struct {
	Status string     `json:"status"`
	Guide  string     `json:"guide"` // concise markdown routing guide
	Tools  []NavEntry `json:"tools"` // structured tool list
}

// allNavEntries is the full catalogue ordered from most-used to least.
var allNavEntries = []NavEntry{
	{
		Name:    "ask",
		Purpose: "Primary code-understanding entry point — composes semantic search, symbol lookup, graph expansion, and optionally synthesized prose answer in one call.",
		When:    "Before any Grep/Read/Glob fan-out on a code question: where, how, callers, architecture, editing context. Follow next_action; honor avoid.",
	},
	{
		Name:    "session",
		Purpose: "Per-project session memory — task declaration, findings, files read/written.",
		When:    "set_task at session start; add_note when you find something important; get to recover state after a reconnect.",
	},
	{
		Name:    "notes",
		Purpose: "Persistent project knowledge store — facts, patterns, gotchas that survive session resets.",
		When:    "add: record an architectural decision or gotcha. list: retrieve high-salience facts before starting a task.",
	},
	{
		Name:    "shell",
		Purpose: "Execute a shell command and return compressed output (strips build noise, collapses log lines).",
		When:    "When you need to run a build, test, or git command without burning context budget on raw output.",
	},
	{
		Name:    "ls",
		Purpose: "List indexed files under a directory path with chunk counts.",
		When:    "Orientation in an unfamiliar subtree. No embedding required — instant from the index.",
	},
	{
		Name:    "grep",
		Purpose: "Regex search over project files — no embedding required.",
		When:    "Exact-match queries: cross-cutting symbol references, import paths, string literals. Use ask for conceptual queries.",
	},
	{
		Name:    "find",
		Purpose: "Hybrid BM25+vector search — embeds query, returns top-k matching chunks with RRF fusion.",
		When:    "Conceptual or keyword search when ask's full routing overhead is unnecessary.",
	},
	{
		Name:    "lookup",
		Purpose: "Fast SQL exact-identifier lookup — no embedding.",
		When:    "You have the exact identifier name and want nothing else. ask runs this automatically.",
	},
	{
		Name:    "read",
		Purpose: "Send a file slice to the chat model for a digested summary (signatures, structure, gist).",
		When:    "You already know which file to digest. Prefer ask's suggested_reads first — they name the right file.",
	},
	{
		Name:    "deps",
		Purpose: "Import edges for a file or package from the static graph.",
		When:    "Direct dependency inspection without ask overhead.",
	},
	{
		Name:    "callers",
		Purpose: "Functions that CALL the given symbol (Go call graph).",
		When:    "Direct call-chain inspection. ask uses callers intent automatically.",
	},
	{
		Name:    "callees",
		Purpose: "Functions that the given symbol CALLS (Go call graph).",
		When:    "Direct call-chain inspection. ask uses callees intent automatically.",
	},
	{
		Name:    "impact",
		Purpose: "Transitive blast-radius analysis — follows call edges outward up to max_depth.",
		When:    "Before editing a widely-called symbol to gauge ripple. Use ask with callers intent first.",
	},
	{
		Name:    "routes",
		Purpose: "Detect HTTP handlers, MCP tool registrations, and gRPC service impls from the call graph.",
		When:    "Understanding a service's public surface before reading its handlers.",
	},
	{
		Name:    "smells",
		Purpose: "AST-based code quality signals: long functions, dead exports, god files.",
		When:    "Before a PR or refactor to spot obvious structural issues.",
	},
	{
		Name:    "path",
		Purpose: "Shortest call/import path between two symbols or files.",
		When:    "Tracing how A reaches B without reading every intermediate file.",
	},
	{
		Name:    "diff",
		Purpose: "Blast-radius of a git diff — which symbols are touched and who calls them.",
		When:    "Before pushing a change to understand the full impact surface.",
	},
	{
		Name:    "clusters",
		Purpose: "Louvain community detection on the call graph — surfaces module-level clusters.",
		When:    "Understanding high-level module structure of an unfamiliar codebase.",
	},
	{
		Name:    "status",
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
- You're hunting an exact literal (error msg, magic number) → **grep**.
- You're editing → **Edit** (native).

## Standard tool routing

| Question type | Tool |
|---|---|
| Where / how / callers / architecture / editing context | **ask** |
| What files are in this dir? | **ls** |
| Exact regex match across the codebase | **grep** |
| Digest a known file (signatures, structure) | **read** |
| Store a fact / gotcha for future sessions | **notes** add |
| Remember what you're working on | **session** set_task |
| Run a command without burning context | **shell** |

## Fallback behaviors

- ask returns **status: "no-index"** → run ` + "`dex index <project>`" + ` once, fall back to grep.
- ask returns **status: "embedding-service-unreachable"** → embed is offline; use grep.
- ask returns **stale: true** → results are behind HEAD; flag that fixes may depend on recent code.
`

func (s *Server) nav(_ context.Context, _ *sdk.CallToolRequest, _ NavInput) (*sdk.CallToolResult, NavOutput, error) {
	out := NavOutput{
		Status: "ok",
		Guide:  navGuide,
		Tools:  allNavEntries,
	}
	// Return the guide as readable markdown in Content; structured tool list is
	// also available via StructuredContent (the SDK fills that from out).
	res := &sdk.CallToolResult{
		Content: []sdk.Content{&sdk.TextContent{Text: navText(out)}},
	}
	return res, out, nil
}

func navText(out NavOutput) string {
	var b strings.Builder
	b.WriteString(out.Guide)
	b.WriteString("\n## Available tools\n\n")
	for _, e := range out.Tools {
		b.WriteString("### ")
		b.WriteString(e.Name)
		b.WriteString("\n")
		b.WriteString(e.Purpose)
		b.WriteString("\n*When:* ")
		b.WriteString(e.When)
		b.WriteString("\n\n")
	}
	return b.String()
}
