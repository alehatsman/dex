package mcp

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/alehatsman/dex/internal/codemap"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// The verb facade (#316 story 3): a small default tool surface — map / find /
// trace / read / ask — that everyday agents reach for, with the
// granular graph/search/analysis lanes moved behind DEX_EXPERT. The facades
// here are thin compositions over the existing toolSurface handlers (no
// handler rewrites): they route an input to the right underlying call and copy
// its result into one verb-shaped envelope. Because they are free functions
// over toolSurface, every backend (local *Server, remote proxy, maintenance,
// http) gets the verb for free — no new interface methods, no new REST routes.

// expertEnabled reports whether the power-tool tier should be registered. The
// default verb surface covers everyday work; operators opt into the raw lanes
// (deps/callers/callees/path/diff/clusters/routes/smells/status/notes/
// session) with DEX_EXPERT. Parsed leniently: any value other than the usual
// falsey strings enables it.
func expertEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DEX_EXPERT"))) {
	case "", "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// MapInput drives the map verb — a deterministic, zero-inference orientation
// map built from the pre-computed Louvain communities + PageRank already in the
// graph (epic #316 story 1). With no Cluster it renders the L0 overview (top
// clusters); with Cluster set it zooms into that one cluster (L1).
type MapInput struct {
	Cluster     *int   `json:"cluster,omitempty" jsonschema:"cluster id to zoom into (L1 detail); omit for the repo overview (L0)"`
	Budget      int    `json:"budget,omitempty" jsonschema:"token budget for the rendered map (default 150 for L0, 1000 for L1)"`
	MinMembers  int    `json:"min_members,omitempty" jsonschema:"min cluster size to consider (default 3)"`
	K           int    `json:"k,omitempty" jsonschema:"max clusters to scan (default 50)"`
	TopK        int    `json:"top_k,omitempty" jsonschema:"max symbols pulled per cluster (default 25)"`
	Around      string `json:"around,omitempty" jsonschema:"render a task-focused region around this symbol — its callers ∪ callees — instead of the repo overview; mutually exclusive with cluster and around_diff"`
	AroundDiff  string `json:"around_diff,omitempty" jsonschema:"render the blast radius of a git diff: the ref to diff against (e.g. 'HEAD~1'); mutually exclusive with cluster and around"`
	Task        string `json:"task,omitempty" jsonschema:"current task description — when set, every indexed file is scored against this task and returned as l0_files/l1_files/l2_count with per-file recommended_mode; requires an embed client (degrades to the normal topology map when none is wired)"`
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project or git worktree you are working in. The server cannot see your shell's directory; when working in a worktree different from where the server started, pass that worktree's path"`
}

// TaskFile is one entry in the task-filtered read list returned by map when
// task is set (#609). Score is the cosine similarity (boosted by git recency
// and session bounce) and Mode is the recommended read mode.
type TaskFile struct {
	Path  string  `json:"path"`
	Score float32 `json:"score"`
	Mode  string  `json:"mode"` // "full" | "signatures" | "skeleton"
}

// MapOutput carries the rendered markdown map plus a status. Map holds the same
// text a human sees from `dex map`; agents can read it directly. When task is
// set the task-filtered fields (L0Files/L1Files/L2Count/GitBoosted) are
// populated instead.
type MapOutput struct {
	Status string `json:"status"` // "ok" | "no-index" | "no-graph" | "not-found" | "error"
	Hint   string `json:"hint,omitempty"`
	Zoom   string `json:"zoom,omitempty"` // "orient" | "l1" | "around" | "task"
	Map    string `json:"map,omitempty"`
	// Task-filtered fields — populated when MapInput.Task is set.
	Task       string     `json:"task,omitempty"`
	L0Files    []TaskFile `json:"l0_files,omitempty"`
	L1Files    []TaskFile `json:"l1_files,omitempty"`
	L2Count    int        `json:"l2_count,omitempty"`
	GitBoosted []string   `json:"git_boosted,omitempty"`
}

// mapHandler adapts mapVerb to the SDK handler shape, capturing h.
func mapHandler(h toolSurface) func(context.Context, *sdk.CallToolRequest, MapInput) (*sdk.CallToolResult, MapOutput, error) {
	return func(ctx context.Context, req *sdk.CallToolRequest, in MapInput) (*sdk.CallToolResult, MapOutput, error) {
		return mapVerb(ctx, h, req, in)
	}
}

// Map runs the map verb for callers without an SDK request — the REST `/map`
// route. It composes over the local *Server exactly like the stdio `map` tool,
// so both transports agree.
func (s *Server) Map(ctx context.Context, in MapInput) (MapOutput, error) {
	_, out, err := mapVerb(ctx, s, nil, in)
	return out, err
}

// mapVerb composes the existing community projection (the `clusters` lane) with
// the codemap renderer — no model is called. It mirrors the assembly in
// `dex map` (cmd/dex/map.go) so the MCP verb and the CLI agree.
func mapVerb(ctx context.Context, h toolSurface, req *sdk.CallToolRequest, in MapInput) (*sdk.CallToolResult, MapOutput, error) {
	// #609: task-filtered read list. When a task is provided and the handler is
	// a *Server (so we have embed + store access), score every indexed file and
	// return L0/L1/L2 buckets with per-file recommended_mode. Degrades to the
	// normal topology map when no embedder is wired or on non-*Server surfaces.
	if in.Task != "" {
		if srv, ok := h.(*Server); ok {
			return srv.taskMap(ctx, in)
		}
	}
	// #347 story 5: task-conditioned region. `around`/`around_diff` render the
	// call-graph neighborhood of a symbol or the blast radius of a diff instead
	// of the Louvain L0/L1 overview, so they branch off before the community
	// projection. The two are mutually exclusive with each other and with
	// `cluster` (which zooms a community, a different notion of region).
	if in.Around != "" || in.AroundDiff != "" {
		if in.Around != "" && in.AroundDiff != "" {
			return nil, MapOutput{Status: "error", Hint: "around and around_diff are mutually exclusive — pass one"}, nil
		}
		if in.Cluster != nil {
			return nil, MapOutput{Status: "error", Hint: "around cannot be combined with cluster — cluster zooms a Louvain community; around renders a call-graph or diff region"}, nil
		}
		return mapAround(ctx, h, req, in)
	}

	minMembers, k, topK := in.MinMembers, in.K, in.TopK
	if minMembers == 0 {
		minMembers = 3
	}
	if k == 0 {
		k = 50
	}
	if topK == 0 {
		topK = 25
	}
	_, comm, err := h.graphCommunities(ctx, req, CommunitiesInput{
		MinMembers:  minMembers,
		K:           k,
		TopK:        topK,
		ProjectRoot: in.ProjectRoot,
	})
	if err != nil {
		return nil, MapOutput{Status: "error", Hint: err.Error()}, err
	}
	if comm.Status != "ok" {
		return nil, MapOutput{Status: comm.Status, Hint: comm.Hint}, nil
	}
	if len(comm.Communities) == 0 && comm.Hint != "" {
		return nil, MapOutput{Status: "ok", Hint: comm.Hint}, nil
	}

	clusters := AdaptCommunities(comm.Communities)
	if in.Cluster != nil {
		c, ok := findCluster(clusters, *in.Cluster)
		if !ok {
			return nil, MapOutput{Status: "not-found", Hint: fmt.Sprintf("cluster #%d not found (omit `cluster` to list clusters)", *in.Cluster)}, nil
		}
		return nil, MapOutput{Status: "ok", Zoom: "l1", Map: codemap.RenderL1(c, in.Budget)}, nil
	}
	// Default (no cluster): the first-touch orientation bundle — L0 overview plus
	// an auto-zoom into the most-central cluster (#574, the former `orient`).
	// RenderOrient defaults the budgets when zero (150 L0, 1000 L1).
	return nil, MapOutput{Status: "ok", Zoom: "orient", Map: codemap.RenderOrient(clusters,
		codemap.OrientExtras{Entrypoints: comm.Entrypoints, Commands: ExtractProjectCommands(comm.Project), ImportEdges: CodemapImportEdges(comm.ImportEdges), Externals: comm.Externals, Scale: CodemapScale(comm.Scale)}, in.Budget, in.Budget)}, nil
}

// AdaptCommunities maps the MCP community projection into the renderer's input.
// cmd/dex delegates here so the logic lives in one place; it cannot live in
// codemap because codemap is imported by mcp, which would create a cycle.
func AdaptCommunities(comms []Community) []codemap.Cluster {
	clusters := make([]codemap.Cluster, 0, len(comms))
	for _, c := range comms {
		syms := make([]codemap.Symbol, 0, len(c.Members))
		for _, m := range c.Members {
			syms = append(syms, codemap.Symbol{
				QualifiedName: m.QualifiedName,
				Kind:          m.Kind,
				Pkg:           m.Package,
				Path:          m.Path,
				Line:          m.StartLine,
				PageRank:      m.PageRank,
			})
		}
		clusters = append(clusters, codemap.Cluster{ID: c.ID, Size: c.Size, Symbols: syms})
	}
	return clusters
}

func findCluster(clusters []codemap.Cluster, id int) (codemap.Cluster, bool) {
	for _, c := range clusters {
		if c.ID == id {
			return c, true
		}
	}
	return codemap.Cluster{}, false
}
