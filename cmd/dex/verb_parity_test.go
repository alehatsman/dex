package main

import "testing"

// mcpToolSurface is the MCP tool surface, kept in lockstep with registerTools
// (internal/mcp/server.go) by hand. Both parity tests read from this single
// list so the CLI↔MCP contract is guarded in both directions.
var mcpToolSurface = []string{
	"ask", "find", "lookup", "map", "trace", "locate", "review", "refactor", "read",
	"grep", "ls", "shell",
	"deps", "diff", "clusters",
	"smells", "routes", "cohort",
	"status", "notes", "session", "budget", "checkpoint",
}

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
	mcpTools := mcpToolSurface

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

	mcpOnlyTools := map[string]bool{
		"session": true,
		// budget reports per-session counters (slo.Tracker + heatmap) — a CLI
		// invocation has no agent session and no in-memory counters to report.
		"budget": true,
		// checkpoint manages an agent's shadow-git work history — a session-scoped
		// concept with no CLI analogue (#608), surfaced via mcpOnlyToolHints.
		"checkpoint": true,
	}

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

// TestQueryVerbsHaveMCPTool is the reverse guard of TestMCPToolCLIParity: every
// groupQuery CLI verb must map to an MCP tool of the same name. This is the
// check that the `orient` verb (#574) slipped past — it shipped as a query verb
// with no MCP tool and no docs entry. A query verb that is deliberately
// CLI-only must be added to cliOnlyQueryVerbs with a reason, so the exception is
// explicit rather than silent.
//
// Lifecycle/ops verbs (groupBuild/groupConfig) are intentionally CLI-only and
// out of scope here — they build, serve, and maintain the index rather than
// query it.
func TestQueryVerbsHaveMCPTool(t *testing.T) {
	mcpSet := map[string]bool{}
	for _, tool := range mcpToolSurface {
		mcpSet[tool] = true
	}

	// cliOnlyQueryVerbs are query verbs that deliberately have no MCP tool.
	// Empty today — every query verb mirrors an MCP tool. Add with a reason.
	cliOnlyQueryVerbs := map[string]bool{}

	for _, v := range verbs {
		if v.group != groupQuery {
			continue
		}
		if mcpSet[v.name] || cliOnlyQueryVerbs[v.name] {
			continue
		}
		t.Errorf("query verb %q has no MCP tool and is not in cliOnlyQueryVerbs — "+
			"either add the MCP tool (keep CLI↔MCP parity) or allow-list it with a reason", v.name)
	}

	// Guard the allow-list against staleness.
	for verb := range cliOnlyQueryVerbs {
		if mcpSet[verb] {
			t.Errorf("cliOnlyQueryVerbs lists %q but it is now an MCP tool — stale allow-list", verb)
		}
	}
}
