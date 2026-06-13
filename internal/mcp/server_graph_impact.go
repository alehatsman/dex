package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/alehatsman/dex/internal/graph"
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
	if len(view.edgesByKind[graph.EdgeCalls]) == 0 {
		return nil, ImpactOutput{Status: "no-graph", Project: p.Root,
			Hint: "graph has no `calls` edges — reindex with this release (`dex index . --graph=only`) to extract them."}, nil
	}

	targets := resolveCallTargets(view, in.Name, in.Package)
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
	nodes := computeImpactNodes(view, targets, maxDepth)
	out.Total = len(nodes)
	if len(nodes) > maxImpactNodes {
		nodes = nodes[:maxImpactNodes]
		out.Truncated = true
	}
	out.Nodes = nodes
	return nil, out, nil
}

// computeImpactNodes performs a BFS over incoming calls edges (callers
// direction) starting from seeds, up to maxDepth hops. Returns nodes
// sorted by depth asc, PageRank desc, then path+line for determinism.
// Pure over view — unit-testable without a store.
func computeImpactNodes(view *graphView, seeds []graphNode, maxDepth int) []ImpactNode {
	type item struct {
		id    string
		depth int
	}
	visited := map[string]bool{}
	for _, t := range seeds {
		visited[t.ID] = true
	}
	queue := make([]item, 0, len(seeds))
	for _, t := range seeds {
		queue = append(queue, item{t.ID, 0})
	}

	var nodes []ImpactNode
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur.depth >= maxDepth {
			continue
		}
		for _, e := range view.edgesByDst[cur.id] {
			if e.Kind != graph.EdgeCalls {
				continue
			}
			if visited[e.SrcID] {
				continue
			}
			visited[e.SrcID] = true
			caller, ok := view.nodesByID[e.SrcID]
			if !ok {
				continue
			}
			nodes = append(nodes, ImpactNode{
				QualifiedName: caller.QualifiedName,
				Package:       caller.PackagePath,
				Kind:          string(caller.Kind),
				Path:          caller.FilePath,
				StartLine:     caller.StartLine,
				Depth:         cur.depth + 1,
				PageRank:      caller.PageRank,
			})
			queue = append(queue, item{e.SrcID, cur.depth + 1})
		}
	}

	sort.Slice(nodes, func(i, j int) bool {
		a, b := nodes[i], nodes[j]
		if a.Depth != b.Depth {
			return a.Depth < b.Depth
		}
		if a.PageRank != b.PageRank {
			return a.PageRank > b.PageRank
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.StartLine < b.StartLine
	})
	return nodes
}
