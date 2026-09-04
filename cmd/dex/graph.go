package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"

	"github.com/alehatsman/dex/internal/embed"
	"github.com/alehatsman/dex/internal/graph"
	"github.com/alehatsman/dex/internal/graphrefresh"
	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/store"
)

// cmdGraph dispatches `dex graph <subcommand>`. callers/callees/path are
// reached via `dex trace --dir …`, not as graph subs (#728).
func cmdGraph(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("graph needs a subcommand: neighbors | similar | clones | deps | packages | links | backlinks | tags | cycles | diff | clusters | smells | routes | export")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "index":
		return fmt.Errorf("`graph index` has been folded into `index` — use `dex index --graph=only <path>` (or just `dex index <path>`, which runs both phases)")
	case "callers", "callees", "path":
		return fmt.Errorf("`graph %s` is now `dex trace --dir %s` (#728)", sub, sub)
	case "neighbors":
		return cmdGraphNeighbors(ctx, rest)
	case "similar":
		return cmdGraphSimilar(ctx, rest)
	case "clones":
		return cmdGraphClones(ctx, rest)
	case "deps":
		return cmdGraphDeps(ctx, rest)
	case "packages":
		return cmdGraphPackages(ctx, rest)
	case "links":
		return cmdGraphLinks(ctx, rest)
	case "backlinks":
		return cmdGraphBacklinks(ctx, rest)
	case "tags":
		return cmdGraphTags(ctx, rest)
	case "cycles":
		return cmdGraphCycles(ctx, rest)
	case "diff":
		return cmdGraphDiff(ctx, rest)
	case "clusters":
		return cmdGraphCommunities(ctx, rest)
	case "smells":
		return cmdGraphSmells(ctx, rest)
	case "routes":
		return cmdGraphRoutes(ctx, rest)
	case "export":
		return cmdGraphExport(ctx, rest)
	case "-h", "--help", "help":
		fmt.Fprintln(os.Stderr, `usage:
  dex graph neighbors   [<path>] <file> <line>  vector neighbours of a chunk (CLI-only)
  dex graph similar     [<path>] <file> <line>  blocks semantically near a block (MCP: similar, DEX_EXPERT)
                                                    --k=<n>  --threshold=<0..1>
  dex graph clones      [<path>]                clusters of near-duplicate blocks (MCP: clones, DEX_EXPERT)
                                                    --path=<prefix>  --threshold=<0..1>
                                                    --min-lines=<n>  --k=<n>  --max-clusters=<n>
  dex graph deps        [<path>] <file|package>  imports edges (MCP: deps, DEX_EXPERT)
                                                    --file=<rel>  --package=<full>
  dex graph packages    [<path>]                whole internal package import DAG (CLI-only)
  dex graph links       [<path>] <doc>          docs this doc links to (CLI-only)
                                                    --k=<n>
  dex graph backlinks   [<path>] <doc>          docs that link to this doc (CLI-only)
                                                    --k=<n>
  dex graph tags        [<path>] [--tag=<t>|--doc=<d>]
                                                    tag→docs or doc→tags (CLI-only)
                                                    --k=<n>
  dex graph cycles      [<path>]                call-graph SCCs ≥ size 2 (CLI-only)
                                                    --min-size=<n>  --k=<n>
  dex graph diff        [<path>]                blast-radius of current git diff (CLI-only;
                                                    MCP covers this via review_diff / trace --dir impact, DEX_EXPERT)
                                                    --ref=<ref>  --depth=<n>
  dex graph clusters    [<path>]                Louvain call/import-graph clusters (MCP: clusters, DEX_EXPERT)
                                                    --min-members=<n>  --k=<n>  --top-k=<n>
  dex graph smells      [<path>]                long funcs, dead exports, god files/nodes (MCP: smells, DEX_EXPERT)
                                                    --min-func-lines=<n>  --min-file-symbols=<n>
                                                    --min-god-node-callers=<n>  --limit=<n>
  dex graph routes      [<path>]                HTTP/MCP/gRPC handlers + registration sites (MCP: routes, DEX_EXPERT)
  dex graph export      [<path>] [--output=<dir>]
                                                    dump nodes/edges as JSONL (CLI-only)
  (path defaults to cwd when omitted)

note:
  callers/callees/path fold into 'dex trace --dir callers|callees|path' (MCP: trace, DEX_EXPERT).
  'graph index' is gone — use 'dex index --graph=only <path>'.
  Plain 'dex index <path>' runs both chunk and graph phases.
  Everyday MCP agents: none of these are on the default surface — 'query' covers
  this work; the tools named above require DEX_EXPERT=1.`)
		return nil
	default:
		return fmt.Errorf("unknown graph subcommand: %s (have: neighbors, similar, clones, deps, packages, links, backlinks, tags, cycles, diff, clusters, smells, routes, export)", sub)
	}
}

// parsePositiveInt is a tiny CLI helper for arg-parsing positional
// integers (e.g. `<line>`). Returns an error with the flag/arg name so
// the user knows which token failed.
func parsePositiveInt(name, raw string) (int, error) {
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer (got %q)", name, raw)
	}
	return v, nil
}

// graphIndexResult is the JSON payload emitted by `index --graph=only --format=json`.
type graphIndexResult struct {
	Project    string   `json:"project"`
	Packages   int      `json:"packages"`
	Nodes      int64    `json:"nodes"`
	Edges      int64    `json:"edges"`
	Pruned     int64    `json:"pruned_nodes"`
	PrunedEdge int64    `json:"pruned_edges"`
	Linked     int      `json:"linked_to_chunks"`
	ElapsedMS  int64    `json:"elapsed_ms"`
	Warnings   []string `json:"warnings,omitempty"`
}

// runGraphPhase extracts the Go static graph for p and upserts into st.
// Shared by `index` (Phase 2) and `index --graph=only`.
func runGraphPhase(ctx context.Context, p *proj.Project, st *store.Store, verbose bool) (*graph.Stats, error) {
	return graphrefresh.RunPhase(ctx, p, st, verbose, cliLogger())
}

// reportGraphStats prints either a text summary or a JSON blob matching
// the old `graph index --format=json` schema, so existing scripts can
// migrate to `index --graph=only --format=json` without a payload change.
func reportGraphStats(project string, stats *graph.Stats, format string) error {
	switch format {
	case "json":
		out := graphIndexResult{
			Project:    project,
			Packages:   stats.Packages,
			Nodes:      stats.NodesUpserted,
			Edges:      stats.EdgesUpserted,
			Pruned:     stats.NodesPruned,
			PrunedEdge: stats.EdgesPruned,
			Linked:     stats.LinkedToChunks,
			ElapsedMS:  stats.Elapsed.Milliseconds(),
			Warnings:   stats.Warnings,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	default:
		fmt.Fprintf(os.Stderr, "  graph: %d packages  %d nodes  %d edges  %d linked  pruned %d/%d  in %s\n",
			stats.Packages, stats.NodesUpserted, stats.EdgesUpserted,
			stats.LinkedToChunks, stats.NodesPruned, stats.EdgesPruned, stats.Elapsed)
		if len(stats.Warnings) > 0 {
			fmt.Fprintf(os.Stderr, "  warnings: %d\n", len(stats.Warnings))
			for _, w := range stats.Warnings {
				fmt.Fprintf(os.Stderr, "    %s\n", w)
			}
		}
		return nil
	}
}

// embedGraphNodes embeds all graph_nodes whose vec_hash differs from
// content_hash (un-embedded or stale). Returns the number of nodes embedded.
func embedGraphNodes(ctx context.Context, st *store.Store, em embed.Embedder, verbose bool, logger *slog.Logger) (int, error) {
	return graphrefresh.EmbedNodes(ctx, st, em, verbose, logger)
}
