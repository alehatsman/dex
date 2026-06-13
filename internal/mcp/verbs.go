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
// trace / impact / read / ask — that everyday agents reach for, with the
// granular graph/search/analysis lanes moved behind DEX_EXPERT. The facades
// here are thin compositions over the existing toolSurface handlers (no
// handler rewrites): they route an input to the right underlying call and copy
// its result into one verb-shaped envelope. Because they are free functions
// over toolSurface, every backend (local *Server, remote proxy, maintenance,
// http) gets the verb for free — no new interface methods, no new REST routes.

// expertEnabled reports whether the power-tool tier should be registered. The
// default verb surface covers everyday work; operators opt into the raw lanes
// (lookup/deps/callers/callees/path/diff/clusters/routes/smells/status/notes/
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
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
}

// MapOutput carries the rendered markdown map plus a status. Map holds the same
// text a human sees from `dex map`; agents can read it directly.
type MapOutput struct {
	Status string `json:"status"` // "ok" | "no-index" | "no-graph" | "not-found" | "error"
	Hint   string `json:"hint,omitempty"`
	Zoom   string `json:"zoom,omitempty"` // "l0" | "l1" | "around"
	Map    string `json:"map,omitempty"`
}

// mapHandler adapts mapVerb to the SDK handler shape, capturing h.
func mapHandler(h toolSurface) func(context.Context, *sdk.CallToolRequest, MapInput) (*sdk.CallToolResult, MapOutput, error) {
	return func(ctx context.Context, req *sdk.CallToolRequest, in MapInput) (*sdk.CallToolResult, MapOutput, error) {
		return mapVerb(ctx, h, req, in)
	}
}

// mapVerb composes the existing community projection (the `clusters` lane) with
// the codemap renderer — no model is called. It mirrors the assembly in
// `dex map` (cmd/dex/map.go) so the MCP verb and the CLI agree.
func mapVerb(ctx context.Context, h toolSurface, req *sdk.CallToolRequest, in MapInput) (*sdk.CallToolResult, MapOutput, error) {
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

	clusters := adaptCommunities(comm.Communities)
	if in.Cluster != nil {
		c, ok := findCluster(clusters, *in.Cluster)
		if !ok {
			return nil, MapOutput{Status: "not-found", Hint: fmt.Sprintf("cluster #%d not found (omit `cluster` to list clusters)", *in.Cluster)}, nil
		}
		return nil, MapOutput{Status: "ok", Zoom: "l1", Map: codemap.RenderL1(c, in.Budget)}, nil
	}
	return nil, MapOutput{Status: "ok", Zoom: "l0", Map: codemap.RenderL0(clusters, in.Budget)}, nil
}

// adaptCommunities maps the MCP community projection into the renderer's input.
// (Mirrors cmd/dex/map.go; the adapter must live here, not in codemap, because
// codemap is imported by mcp — referencing mcp.Community there would cycle.)
func adaptCommunities(comms []Community) []codemap.Cluster {
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

// TraceInput drives the trace verb — a single entry point for call-graph
// navigation from a symbol. direction selects the traversal; the call-edge
// directions (callers/callees) and path share one symbol-name input.
type TraceInput struct {
	Symbol      string `json:"symbol" jsonschema:"symbol to trace: bare ('Foo'), receiver-qualified ('(*Server).Run'), or package-tail-qualified ('mcp.NewServer')"`
	Direction   string `json:"direction,omitempty" jsonschema:"'callers' (default — who calls it), 'callees' (what it calls), or 'path' (shortest call route to the 'to' symbol)"`
	To          string `json:"to,omitempty" jsonschema:"destination symbol; required when direction=path"`
	Package     string `json:"package,omitempty" jsonschema:"optional package-path filter when the same name is defined in multiple packages"`
	MaxDepth    int    `json:"max_depth,omitempty" jsonschema:"path BFS depth limit (default 8, max 15); used only when direction=path"`
	K           int    `json:"k,omitempty" jsonschema:"max call-edge hits to return (default 12, max 50); used for callers/callees"`
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
}

// TraceOutput is the unified envelope across the three directions. The
// call-edge directions fill Targets+Hits; path fills Src/Dst/Path. Empty
// fields are omitted, so each direction's response stays compact.
type TraceOutput struct {
	Direction string        `json:"direction"`
	Status    string        `json:"status"` // "ok" | "no-index" | "no-graph" | "not-found" | "no-path" | "error"
	Hint      string        `json:"hint,omitempty"`
	Project   string        `json:"project,omitempty"`
	Targets   []TargetMatch `json:"targets,omitempty"` // callers/callees: resolved interpretations of `symbol`
	Hits      []CallSite    `json:"hits,omitempty"`    // callers/callees: the call-edge endpoints
	Src       string        `json:"src,omitempty"`     // path
	Dst       string        `json:"dst,omitempty"`     // path
	Path      []PathHop     `json:"path,omitempty"`    // path: ordered hops
}

// traceHandler adapts traceVerb to the SDK handler shape, capturing h.
func traceHandler(h toolSurface) func(context.Context, *sdk.CallToolRequest, TraceInput) (*sdk.CallToolResult, TraceOutput, error) {
	return func(ctx context.Context, req *sdk.CallToolRequest, in TraceInput) (*sdk.CallToolResult, TraceOutput, error) {
		return traceVerb(ctx, h, req, in)
	}
}

// traceVerb dispatches a trace call to the underlying graph handler for the
// requested direction and folds its result into TraceOutput.
func traceVerb(ctx context.Context, h toolSurface, req *sdk.CallToolRequest, in TraceInput) (*sdk.CallToolResult, TraceOutput, error) {
	dir := strings.ToLower(strings.TrimSpace(in.Direction))
	if dir == "" {
		dir = "callers"
	}
	switch dir {
	case "callers", "callees":
		ce := CallEdgeInput{Name: in.Symbol, Package: in.Package, ProjectRoot: in.ProjectRoot, K: in.K}
		var out CallEdgeOutput
		var err error
		if dir == "callers" {
			_, out, err = h.graphCallers(ctx, req, ce)
		} else {
			_, out, err = h.graphCallees(ctx, req, ce)
		}
		return nil, TraceOutput{
			Direction: dir,
			Status:    out.Status,
			Hint:      out.Hint,
			Project:   out.Project,
			Targets:   out.Targets,
			Hits:      out.Hits,
		}, err
	case "path":
		if strings.TrimSpace(in.To) == "" {
			return nil, TraceOutput{Direction: dir, Status: "error", Hint: "direction=path requires `to` (destination symbol)"}, nil
		}
		pi := PathInput{Src: in.Symbol, Dst: in.To, Package: in.Package, MaxDepth: in.MaxDepth, ProjectRoot: in.ProjectRoot}
		_, out, err := h.graphPath(ctx, req, pi)
		return nil, TraceOutput{
			Direction: dir,
			Status:    out.Status,
			Hint:      out.Hint,
			Project:   out.Project,
			Src:       out.Src,
			Dst:       out.Dst,
			Path:      out.Path,
		}, err
	default:
		return nil, TraceOutput{Direction: dir, Status: "error", Hint: "direction must be one of: callers, callees, path"}, nil
	}
}
