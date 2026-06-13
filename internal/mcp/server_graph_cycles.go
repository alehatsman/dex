package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/alehatsman/dex/internal/graph"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ─── tool: graph_cycles ───────────────────────────────────────────────────

type CyclesInput struct {
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
	MinSize     int    `json:"min_size,omitempty" jsonschema:"minimum SCC size to include (default 2 — only cycles, not trivially-acyclic nodes)"`
	K           int    `json:"k,omitempty" jsonschema:"max cycles to return (default 20, max 100)"`
}

// CycleNode is one node in a strongly connected component (call cycle).
type CycleNode struct {
	QualifiedName string `json:"qualified_name"`
	Package       string `json:"package,omitempty"`
	Kind          string `json:"kind"`
	Path          string `json:"path,omitempty"`
	StartLine     int    `json:"start_line,omitempty"`
}

// Cycle is one strongly connected component of size ≥ minSize.
type Cycle struct {
	Size  int         `json:"size"`
	Nodes []CycleNode `json:"nodes"`
}

type CyclesOutput struct {
	Status  string  `json:"status"` // "ok" | "no-index" | "no-graph" | "error"
	Hint    string  `json:"hint,omitempty"`
	Project string  `json:"project,omitempty"`
	Total   int     `json:"total"` // total SCCs found (before K cap)
	Cycles  []Cycle `json:"cycles,omitempty"`
}

func (s *Server) GraphCycles(ctx context.Context, in CyclesInput) (CyclesOutput, error) {
	_, out, err := s.graphCycles(ctx, nil, in)
	return out, err
}

func (s *Server) graphCycles(ctx context.Context, _ *sdk.CallToolRequest, in CyclesInput) (*sdk.CallToolResult, CyclesOutput, error) {
	p, hint := s.resolveProject(in.ProjectRoot)
	if hint != "" {
		return nil, CyclesOutput{Status: "error", Hint: hint}, nil
	}
	if _, err := os.Stat(p.DBPath); errors.Is(err, os.ErrNotExist) {
		return nil, CyclesOutput{Status: "no-index", Project: p.Root,
			Hint: fmt.Sprintf("no index for %s — run `dex index %s` first.", p.Root, p.Root)}, nil
	}
	st, err := s.openStore(p.DBPath)
	if err != nil {
		return nil, CyclesOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
	}

	view, err := s.cachedLoadGraphView(ctx, st, p.DBPath)
	if err != nil {
		return nil, CyclesOutput{Status: "error", Hint: fmt.Sprintf("load graph: %v", err)}, nil
	}
	if view == nil {
		return nil, CyclesOutput{Status: "no-graph", Project: p.Root,
			Hint: fmt.Sprintf("graph not indexed for %s — run `dex index %s --graph=only`.", p.Root, p.Root)}, nil
	}
	if len(view.edgesByKind[graph.EdgeCalls]) == 0 {
		return nil, CyclesOutput{Status: "no-graph", Project: p.Root,
			Hint: "graph has no `calls` edges — reindex with this release (`dex index . --graph=only`) to extract them."}, nil
	}

	minSize := in.MinSize
	if minSize < 2 {
		minSize = 2
	}
	k := in.K
	if k <= 0 {
		k = 20
	}
	if k > 100 {
		k = 100
	}

	sccs := buildCycles(view, minSize)
	out := CyclesOutput{Status: "ok", Project: p.Root, Total: len(sccs)}
	if len(sccs) > k {
		sccs = sccs[:k]
	}
	for _, scc := range sccs {
		c := Cycle{Size: len(scc)}
		for _, id := range scc {
			n, ok := view.nodesByID[id]
			if !ok {
				continue
			}
			c.Nodes = append(c.Nodes, CycleNode{
				QualifiedName: n.QualifiedName,
				Package:       n.PackagePath,
				Kind:          string(n.Kind),
				Path:          n.FilePath,
				StartLine:     n.StartLine,
			})
		}
		out.Cycles = append(out.Cycles, c)
	}
	return nil, out, nil
}

// buildCycles computes Tarjan SCCs over the `calls` edges in the view
// and returns IDs of components of size ≥ minSize, sorted by descending
// size. Pure over view — unit-testable.
func buildCycles(view *graphView, minSize int) [][]string {
	nodes := make([]graph.Node, 0, len(view.nodesByID))
	for _, n := range view.nodesByID {
		nodes = append(nodes, graph.Node{
			ID:          n.ID,
			PackagePath: n.PackagePath,
		})
	}
	edges := make([]graph.Edge, 0, len(view.edgesByKind[graph.EdgeCalls]))
	for _, e := range view.edgesByKind[graph.EdgeCalls] {
		edges = append(edges, graph.Edge{
			Kind:  graph.EdgeCalls,
			SrcID: e.SrcID,
			DstID: e.DstID,
		})
	}
	sccs := graph.TarjanSCC(nodes, edges, nil)
	var out [][]string
	for _, scc := range sccs {
		if len(scc.IDs) >= minSize {
			out = append(out, scc.IDs)
		}
	}
	return out
}
