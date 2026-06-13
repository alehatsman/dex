package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/alehatsman/dex/internal/graph"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ─── tool: graph_path ─────────────────────────────────────────────────────

type PathInput struct {
	Src         string `json:"src" jsonschema:"source symbol name (bare, receiver-qualified, or pkg-tail-qualified)"`
	Dst         string `json:"dst" jsonschema:"destination symbol name"`
	Package     string `json:"package,omitempty" jsonschema:"optional package filter applied to both src and dst"`
	MaxDepth    int    `json:"max_depth,omitempty" jsonschema:"BFS depth limit (default 8, max 15)"`
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
}

type PathHop struct {
	QualifiedName string `json:"qualified_name"`
	Package       string `json:"package,omitempty"`
	Kind          string `json:"kind"`
	Path          string `json:"path"`
	StartLine     int    `json:"start_line"`
	EdgeKind      string `json:"edge_kind,omitempty"` // edge leading *into* this hop
}

type PathOutput struct {
	Status   string    `json:"status"` // "ok" | "no-path" | "no-index" | "no-graph" | "not-found" | "error"
	Hint     string    `json:"hint,omitempty"`
	Project  string    `json:"project,omitempty"`
	Src      string    `json:"src,omitempty"`
	Dst      string    `json:"dst,omitempty"`
	MaxDepth int       `json:"max_depth,omitempty"`
	Path     []PathHop `json:"path,omitempty"`
}

func (s *Server) GraphPath(ctx context.Context, in PathInput) (PathOutput, error) {
	_, out, err := s.graphPath(ctx, nil, in)
	return out, err
}

func (s *Server) graphPath(ctx context.Context, _ *sdk.CallToolRequest, in PathInput) (*sdk.CallToolResult, PathOutput, error) {
	if strings.TrimSpace(in.Src) == "" || strings.TrimSpace(in.Dst) == "" {
		return nil, PathOutput{Status: "error", Hint: "src and dst must both be non-empty"}, nil
	}
	p, hint := s.resolveProject(in.ProjectRoot)
	if hint != "" {
		return nil, PathOutput{Status: "error", Hint: hint}, nil
	}
	if _, err := os.Stat(p.DBPath); errors.Is(err, os.ErrNotExist) {
		return nil, PathOutput{Status: "no-index", Project: p.Root,
			Hint: fmt.Sprintf("no index for %s — run `dex index %s` first.", p.Root, p.Root)}, nil
	}
	st, err := s.openStore(p.DBPath)
	if err != nil {
		return nil, PathOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
	}

	view, err := s.cachedLoadGraphView(ctx, st, p.DBPath)
	if err != nil {
		return nil, PathOutput{Status: "error", Hint: fmt.Sprintf("load graph: %v", err)}, nil
	}
	if view == nil {
		return nil, PathOutput{Status: "no-graph", Project: p.Root,
			Hint: fmt.Sprintf("graph not indexed for %s — run `dex index %s --graph=only`.", p.Root, p.Root)}, nil
	}

	srcs := resolveCallTargets(view, in.Src, in.Package)
	if len(srcs) == 0 {
		return nil, PathOutput{Status: "not-found", Project: p.Root,
			Hint: fmt.Sprintf("no graph node matches src=%q", in.Src)}, nil
	}
	dsts := resolveCallTargets(view, in.Dst, in.Package)
	if len(dsts) == 0 {
		return nil, PathOutput{Status: "not-found", Project: p.Root,
			Hint: fmt.Sprintf("no graph node matches dst=%q", in.Dst)}, nil
	}

	maxDepth := in.MaxDepth
	if maxDepth <= 0 {
		maxDepth = 8
	}
	if maxDepth > 15 {
		maxDepth = 15
	}

	dstSet := make(map[string]bool, len(dsts))
	for _, d := range dsts {
		dstSet[d.ID] = true
	}

	hops := bfsPath(view, srcs, dstSet, maxDepth)
	if hops == nil {
		return nil, PathOutput{
			Status: "no-path", Project: p.Root,
			Src: in.Src, Dst: in.Dst, MaxDepth: maxDepth,
			Hint: fmt.Sprintf("no path from %q to %q within depth %d", in.Src, in.Dst, maxDepth),
		}, nil
	}
	return nil, PathOutput{
		Status: "ok", Project: p.Root,
		Src: in.Src, Dst: in.Dst, MaxDepth: maxDepth,
		Path: hops,
	}, nil
}

// bfsPath finds the shortest path from any seed node to any node in dstSet,
// following `calls` and `imports` edges. Returns nil when no path exists
// within maxDepth hops.
func bfsPath(view *graphView, seeds []graphNode, dstSet map[string]bool, maxDepth int) []PathHop {
	type item struct {
		id       string
		depth    int
		prevID   string
		edgeKind graph.EdgeKind
	}

	visited := map[string]bool{}
	parent := map[string]item{}

	queue := make([]item, 0, len(seeds))
	for _, s := range seeds {
		if dstSet[s.ID] {
			// src == dst: trivial path of one hop
			n := view.nodesByID[s.ID]
			return []PathHop{{
				QualifiedName: n.QualifiedName,
				Package:       n.PackagePath,
				Kind:          string(n.Kind),
				Path:          n.FilePath,
				StartLine:     n.StartLine,
			}}
		}
		visited[s.ID] = true
		queue = append(queue, item{id: s.ID, depth: 0})
	}

	var found string
	for len(queue) > 0 && found == "" {
		cur := queue[0]
		queue = queue[1:]
		if cur.depth >= maxDepth {
			continue
		}
		for _, e := range view.edgesBySrc[cur.id] {
			if e.Kind != graph.EdgeCalls && e.Kind != graph.EdgeImports {
				continue
			}
			if visited[e.DstID] {
				continue
			}
			visited[e.DstID] = true
			parent[e.DstID] = item{id: cur.id, depth: cur.depth, edgeKind: e.Kind}
			if dstSet[e.DstID] {
				found = e.DstID
				break
			}
			queue = append(queue, item{id: e.DstID, depth: cur.depth + 1, prevID: cur.id, edgeKind: e.Kind})
		}
	}

	if found == "" {
		return nil
	}

	// Reconstruct path from found back to seed.
	var ids []string
	cur := found
	for {
		ids = append(ids, cur)
		p, ok := parent[cur]
		if !ok {
			break
		}
		cur = p.id
	}
	// Reverse so it reads src → dst.
	for i, j := 0, len(ids)-1; i < j; i, j = i+1, j-1 {
		ids[i], ids[j] = ids[j], ids[i]
	}

	hops := make([]PathHop, 0, len(ids))
	for i, id := range ids {
		n, ok := view.nodesByID[id]
		if !ok {
			continue
		}
		hop := PathHop{
			QualifiedName: n.QualifiedName,
			Package:       n.PackagePath,
			Kind:          string(n.Kind),
			Path:          n.FilePath,
			StartLine:     n.StartLine,
		}
		if i > 0 {
			hop.EdgeKind = string(parent[id].edgeKind)
		}
		hops = append(hops, hop)
	}
	return hops
}
