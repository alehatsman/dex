package main

import "testing"

// TestMCPToolCLIParity locks every MCP `read`/graph/query tool to a reachable
// CLI path (issue #494). grep/ls/shell regressed silently into MCP-only tools
// while the README still advertised them as CLI verbs; this test fails if any
// MCP tool loses its CLI front door again.
//
// The MCP tool surface is the contract here (registerTools in
// internal/mcp/server.go). Each tool is reachable on the CLI in one of two
// shapes, mirrored by the allow-lists below:
//
//   - a top-level verb in the `verbs` registry (the everyday set), or
//   - a `dex graph <sub>` subcommand (the flat graph/analysis tools, by the
//     convention established in #480/#490).
//
// mcpOnlyTools are deliberately MCP-only and need no CLI path:
//   - session: an agent-session lifecycle concept with no CLI analogue.
func TestMCPToolCLIParity(t *testing.T) {
	// The MCP tool surface, kept in lockstep with registerTools by hand.
	mcpTools := []string{
		"ask", "find", "lookup", "map", "trace", "impact", "read",
		"grep", "ls", "shell",
		"deps", "diff", "clusters",
		"smells", "routes",
		"status", "notes", "session",
	}

	topLevel := map[string]bool{}
	graphSubs := map[string]bool{}
	for _, v := range verbs {
		topLevel[v.name] = true
		for _, a := range v.aliases {
			topLevel[a] = true
		}
		if v.name == "graph" {
			for _, s := range v.subs {
				graphSubs[s.name] = true
			}
		}
	}

	mcpOnlyTools := map[string]bool{"session": true}

	for _, tool := range mcpTools {
		if topLevel[tool] || graphSubs[tool] || mcpOnlyTools[tool] {
			continue
		}
		t.Errorf("MCP tool %q has no CLI verb, graph subcommand, or allow-list entry", tool)
	}

	// Guard the allow-list: a stale entry (tool that gained a CLI verb, or was
	// removed) is drift too.
	for tool := range mcpOnlyTools {
		if topLevel[tool] {
			t.Errorf("mcpOnlyTools lists %q but it is now a top-level CLI verb — stale allow-list", tool)
		}
	}
}
