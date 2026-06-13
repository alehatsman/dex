package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/alehatsman/dex/internal/graph"
	"github.com/alehatsman/dex/internal/graphquery"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ─── tools: graph_impact ──────────────────────────────────────────────────

type ImpactInput struct {
	Name        string `json:"name" jsonschema:"symbol to analyse: bare ('Foo'), receiver-qualified ('(*Server).Run'), or package-tail-qualified ('mcp.Server')"`
	Package     string `json:"package,omitempty" jsonschema:"optional package path filter when the same name appears in multiple packages"`
	MaxDepth    int    `json:"max_depth,omitempty" jsonschema:"BFS depth limit (default 3, max 5) — depth 1 = direct callers, depth 2 = their callers, etc."`
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
}

// ImpactNode is one symbol reachable by following callers transitively
// from the seed. Depth 1 = direct callers; depth N = callers N hops out.
type ImpactNode struct {
	QualifiedName string  `json:"qualified_name"`
	Package       string  `json:"package,omitempty"`
	Kind          string  `json:"kind"`
	Path          string  `json:"path"`
	StartLine     int     `json:"start_line"`
	Depth         int     `json:"depth"`
	PageRank      float64 `json:"page_rank,omitempty"`
}

type ImpactOutput struct {
	Status    string        `json:"status"` // "ok" | "no-index" | "no-graph" | "not-found" | "error"
	Hint      string        `json:"hint,omitempty"`
	Project   string        `json:"project,omitempty"`
	Targets   []TargetMatch `json:"targets,omitempty"`
	MaxDepth  int           `json:"max_depth"`
	Total     int           `json:"total"`
	Truncated bool          `json:"truncated,omitempty"`
	Nodes     []ImpactNode  `json:"nodes,omitempty"`
}

func (s *Server) GraphImpact(ctx context.Context, in ImpactInput) (ImpactOutput, error) {
	_, out, err := s.graphImpact(ctx, nil, in)
	return out, err
}

func (s *Server) graphImpact(ctx context.Context, _ *sdk.CallToolRequest, in ImpactInput) (*sdk.CallToolResult, ImpactOutput, error) {
	if strings.TrimSpace(in.Name) == "" {
		return nil, ImpactOutput{Status: "error", Hint: "name is empty"}, nil
	}
	p, hint := s.resolveProject(in.ProjectRoot)
	if hint != "" {
		return nil, ImpactOutput{Status: "error", Hint: hint}, nil
	}
	if _, err := os.Stat(p.DBPath); errors.Is(err, os.ErrNotExist) {
		return nil, ImpactOutput{Status: "no-index", Project: p.Root,
			Hint: fmt.Sprintf("no index for %s — run `dex index %s` first.", p.Root, p.Root)}, nil
	}
	st, err := s.openStore(p.DBPath)
	if err != nil {
		return nil, ImpactOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
	}

	view, err := s.cachedLoadGraphView(ctx, st, p.DBPath)
	if err != nil {
		return nil, ImpactOutput{Status: "error", Hint: fmt.Sprintf("load graph: %v", err)}, nil
	}
	if view == nil {
		return nil, ImpactOutput{Status: "no-graph", Project: p.Root,
			Hint: fmt.Sprintf("graph not indexed for %s — run `dex index %s --graph=only`.", p.Root, p.Root)}, nil
	}
	if len(view.EdgesByKind[graph.EdgeCalls]) == 0 {
		return nil, ImpactOutput{Status: "no-graph", Project: p.Root,
			Hint: "graph has no `calls` edges — reindex with this release (`dex index . --graph=only`) to extract them."}, nil
	}

	targets := graphquery.ResolveCallTargets(view, in.Name, in.Package)
	if len(targets) == 0 {
		return nil, ImpactOutput{Status: "not-found", Project: p.Root,
			Hint: fmt.Sprintf("no graph node matches name=%q", in.Name)}, nil
	}

	maxDepth := in.MaxDepth
	if maxDepth <= 0 {
		maxDepth = 3
	}
	if maxDepth > 5 {
		maxDepth = 5
	}

	out := ImpactOutput{Status: "ok", Project: p.Root, MaxDepth: maxDepth}
	for _, t := range targets {
		out.Targets = append(out.Targets, TargetMatch{
			QualifiedName: t.QualifiedName,
			Package:       t.PackagePath,
			Kind:          string(t.Kind),
			Path:          t.FilePath,
			StartLine:     t.StartLine,
		})
	}

	const maxImpactNodes = 200
	nodes := graphquery.ComputeImpact(view, targets, maxDepth)
	out.Total = len(nodes)
	if len(nodes) > maxImpactNodes {
		nodes = nodes[:maxImpactNodes]
		out.Truncated = true
	}
	out.Nodes = impactNodesFrom(nodes)
	return nil, out, nil
}
