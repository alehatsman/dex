package main

import "testing"

// mcpToolSurface is the MCP tool surface, kept in lockstep with
// registerTools/registerQueryTool (internal/mcp/server_register.go) by hand.
var mcpToolSurface = []string{
	"query", "search", "repo_map", "trace", "locate", "review_diff", "plan_rename", "rehearse_patch", "read",
	"grep",
	"deps", "clusters",
	"smells", "clones", "similar", "routes", "cohort", "refs", "check",
	"status",
	// notes + session were removed from the MCP surface in #195 S4: record covers
	// the write/recall/supersede hot path, notes' admin tail is CLI-only
	// (`dex notes`), and session dedup stays internal (not a verb).
}

// mcpToolQueryKinds maps an MCP tool folded into the CLI's `dex query
// --kind=X` front door (#849 CLI collapse) to the kind(s) that reach it — any
// one kind reaching the door is enough to satisfy parity. Tools not listed
// here are reachable a different way (see cliVerbTools / graphSubTools
// below), not via query at all.
var mcpToolQueryKinds = map[string][]string{
	"search":      {"search"},
	"read":        {"read"},
	"grep":        {"grep"},
	"locate":      {"locate"},
	"trace":       {"symbol", "callers", "callees", "impact", "path"},
	"review_diff": {"review"}, // query kind=review covers the everyday working-tree case only
	"repo_map":    {"orient"}, // bare/default repo_map view only — --cluster/--around have no query facet
	"check":       {"check"},
	"refs":        {"refs"},
	"cohort":      {"cohort"},
	"deps":        {"deps"},
	"status":      {"status"},
}

// cliVerbTools are MCP tools reachable as their own top-level CLI verb rather
// than a query kind — the two "different contract" tools (#849 spec): they
// return an edit plan / hypothetical type-check, not a read.
var cliVerbTools = map[string]bool{
	"plan_rename":    true,
	"rehearse_patch": true,
}

// graphSubTools are MCP tools reachable as a `dex graph <sub>` subcommand —
// the DEX_EXPERT analysis/report lanes that stayed CLI-graph-shaped rather
// than folding into query (#849 spec: zero-subject / whole-repo reports).
var graphSubTools = map[string]bool{
	"clusters": true,
	"smells":   true,
	"clones":   true,
	"similar":  true,
	"routes":   true,
}

// TestMCPToolCLIParity locks every MCP tool to a reachable CLI path (#494,
// redefined by #849's CLI collapse from top-level-verb-name parity to kind=
// ladder coverage — the CLI no longer has a same-named verb per MCP tool,
// `dex query --kind=X` is the front door for most of them). `query` itself is
// trivially reachable (it's the CLI's own top-level verb).
func TestMCPToolCLIParity(t *testing.T) {
	kindSet := map[string]bool{}
	for _, k := range queryKindChoices {
		kindSet[k] = true
	}
	topLevel := map[string]bool{}
	graphSubs := map[string]bool{}
	for _, v := range verbs {
		topLevel[v.name] = true
		if v.name == "graph" {
			for _, s := range v.subs {
				graphSubs[s.name] = true
			}
		}
	}

	for _, tool := range mcpToolSurface {
		switch {
		case tool == "query":
			if !topLevel["query"] {
				t.Errorf("MCP tool %q has no top-level CLI verb", tool)
			}
		case cliVerbTools[tool]:
			if !topLevel[tool] {
				t.Errorf("MCP tool %q is in cliVerbTools but has no top-level CLI verb — stale allow-list", tool)
			}
		case graphSubTools[tool]:
			if !graphSubs[tool] {
				t.Errorf("MCP tool %q is in graphSubTools but `dex graph` has no %q subcommand — stale allow-list", tool, tool)
			}
		default:
			kinds, ok := mcpToolQueryKinds[tool]
			if !ok || len(kinds) == 0 {
				t.Errorf("MCP tool %q has no CLI front door: not query, not in cliVerbTools/graphSubTools, and no mcpToolQueryKinds entry", tool)
				continue
			}
			reachable := false
			for _, k := range kinds {
				if kindSet[k] {
					reachable = true
					break
				}
			}
			if !reachable {
				t.Errorf("MCP tool %q lists query kind(s) %v but none are in query's kind= ladder (queryKindChoices) — stale mapping", tool, kinds)
			}
		}
	}

	// Guard every allow-list against staleness: a tool removed from the MCP
	// surface, or reclassified, should drop out of these maps in the same
	// commit — not linger as a dead entry nobody notices.
	toolSet := map[string]bool{}
	for _, tool := range mcpToolSurface {
		toolSet[tool] = true
	}
	for tool := range mcpToolQueryKinds {
		if !toolSet[tool] {
			t.Errorf("mcpToolQueryKinds lists %q but it is not in mcpToolSurface — stale entry", tool)
		}
	}
	for tool := range cliVerbTools {
		if !toolSet[tool] {
			t.Errorf("cliVerbTools lists %q but it is not in mcpToolSurface — stale entry", tool)
		}
	}
	for tool := range graphSubTools {
		if !toolSet[tool] {
			t.Errorf("graphSubTools lists %q but it is not in mcpToolSurface — stale entry", tool)
		}
	}
}

// TestQueryVerbsHaveMCPTool is the reverse guard: every groupQuery CLI verb
// must map to an MCP tool. Post-#849 there are only three (query,
// plan_rename, rehearse_patch) — the kind= ladder itself is guarded by
// TestMCPToolCLIParity above, not by a per-kind CLI verb anymore.
//
// Lifecycle/ops verbs (groupBuild/groupConfig) are intentionally CLI-only and
// out of scope here — they build, serve, and maintain the index rather than
// query it.
func TestQueryVerbsHaveMCPTool(t *testing.T) {
	mcpSet := map[string]bool{}
	for _, tool := range mcpToolSurface {
		mcpSet[tool] = true
	}
	for _, v := range verbs {
		if v.group != groupQuery {
			continue
		}
		if !mcpSet[v.name] {
			t.Errorf("query verb %q has no MCP tool of the same name", v.name)
		}
	}
}
