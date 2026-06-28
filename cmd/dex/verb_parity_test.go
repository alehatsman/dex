package main

import "testing"

// mcpToolSurface is the MCP tool surface, kept in lockstep with registerTools
// (internal/mcp/server.go) by hand. Both parity tests read from this single
// list so the CLI↔MCP contract is guarded in both directions.
var mcpToolSurface = []string{
	"ask", "search", "repo_map", "trace", "locate", "review_diff", "plan_rename", "rehearse_patch", "read",
	"grep", "shell",
	"deps", "diff", "clusters",
	"smells", "routes", "cohort", "refs", "verify_change", "check",
	"status", "notes", "session", "budget", "checkpoint",
	"brief", "index_status",
}

// cliToMCPName maps CLI verb names to MCP tool names when they differ.
var cliToMCPName = map[string]string{
	"map":      "repo_map",
	"find":     "search",
	"review":   "review_diff",
	"verify":   "verify_change",
	"refactor": "plan_rename",
	"rehearse": "rehearse_patch",
}

// TestMCPToolCLIParity locks every MCP `read`/graph/query tool to a reachable
// CLI path (issue #494). grep/shell regressed silently into MCP-only tools
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

	// mcpToCLIName is the inverse of cliToMCPName: given an MCP tool name,
	// what is the CLI verb name? Built from cliToMCPName at test time.
	mcpToCLIName := make(map[string]string, len(cliToMCPName))
	for cli, mcp := range cliToMCPName {
		mcpToCLIName[mcp] = cli
	}

	mcpOnlyTools := map[string]bool{
		"session": true,
		// budget reports per-session counters (slo.Tracker + heatmap) — a CLI
		// invocation has no agent session and no in-memory counters to report.
		"budget": true,
		// checkpoint manages an agent's shadow-git work history — a session-scoped
		// concept with no CLI analogue (#608), surfaced via mcpOnlyToolHints.
		"checkpoint": true,
		// index_status is surfaced via MCP only; CLI uses `dex index status` / `dex status`.
		"index_status": true,
	}

	for _, tool := range mcpTools {
		if mcpOnlyTools[tool] {
			continue
		}
		// Check direct name match first, then the cliToMCPName mapping (renamed tools).
		cliName := tool
		if mapped, ok := mcpToCLIName[tool]; ok {
			cliName = mapped
		}
		if topLevel[cliName] || graphSubs[cliName] || topLevel[tool] || graphSubs[tool] {
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
	cliOnlyQueryVerbs := map[string]bool{
		// CLI canonical is "xref"; MCP tool is still "refs". The alias on xref
		// covers "refs" in TestMCPToolCLIParity.
		"xref": true,
	}

	for _, v := range verbs {
		if v.group != groupQuery {
			continue
		}
		if cliOnlyQueryVerbs[v.name] {
			continue
		}
		// Check both direct name and the renamed MCP equivalent.
		mcpName := v.name
		if mapped, ok := cliToMCPName[v.name]; ok {
			mcpName = mapped
		}
		if mcpSet[v.name] || mcpSet[mcpName] {
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
