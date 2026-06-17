package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
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
	K           int    `json:"k,omitempty" jsonschema:"max nodes shown per depth (default 8, max 200) — the tail is summarised as a '+N more' line. Set high (e.g. 200) for the full PageRank-sorted list."`
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
	// Elided is a per-depth "+N more at depth D" summary of nodes dropped
	// by the per-depth K cap. Empty when nothing was elided. Same idea as
	// codemap's "+N more" tail line: keep the head readable, summarise the
	// rest instead of dumping it.
	Elided []DepthElision `json:"elided,omitempty"`
}

// DepthElision summarises the PageRank tail dropped at one BFS depth.
type DepthElision struct {
	Depth int `json:"depth"`
	More  int `json:"more"`
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

	// Per-depth cap: ComputeImpact returns nodes sorted (depth asc, PageRank
	// desc), so keeping the first k of each depth yields the top-k hubs at
	// that level and the rest collapse into a "+N more at depth D" line. This
	// mirrors codemap's "+N more" tail elision — a hub symbol with 90 callers
	// no longer dumps 6-8KB of JSON into the context window.
	k := in.K
	if k <= 0 {
		k = 8
	}
	if k > maxImpactNodes {
		k = maxImpactNodes
	}

	nodes := graphquery.ComputeImpact(view, targets, maxDepth)
	out.Total = len(nodes)
	if len(nodes) > maxImpactNodes {
		nodes = nodes[:maxImpactNodes]
		out.Truncated = true
	}

	kept, elided := capPerDepth(nodes, k)
	out.Nodes = impactNodesFrom(kept)
	out.Elided = elided

	// #485: a bare total:0 on a live, exported symbol reads as "dead / safe to
	// delete" — but exported methods are routinely dispatched via an interface
	// or reflection (e.g. the MCP SDK calls tool handlers through toolSurface),
	// which leaves no static `calls` edge. Distinguish "no static callers" from
	// "truly dead" and, where possible, name the interface(s) it satisfies.
	if out.Total == 0 {
		// Impact walks the callers direction transitively. On a name-based
		// (tree-sitter) language, incomplete recall means total:0 is not proof
		// of an empty blast radius — caveat before the Go interface-dispatch hint.
		if h := nameBasedEmptyHint(targets, "callers"); h != "" {
			out.Hint = h
		} else if h := zeroCallerHint(view, targets); h != "" {
			out.Hint = h
		}
	}
	return nil, out, nil
}

const maxImpactNodes = 200

// capPerDepth keeps at most k nodes per BFS depth (nodes must already be sorted
// depth-asc, PageRank-desc) and reports the per-depth tail it dropped.
func capPerDepth(nodes []graphquery.Reachable, k int) ([]graphquery.Reachable, []DepthElision) {
	if k <= 0 {
		return nodes, nil
	}
	kept := make([]graphquery.Reachable, 0, len(nodes))
	perDepth := map[int]int{}
	for _, n := range nodes {
		perDepth[n.Depth]++
		if perDepth[n.Depth] <= k {
			kept = append(kept, n)
		}
	}
	// Build elision lines in depth order from the per-depth counts.
	var elided []DepthElision
	seen := map[int]bool{}
	for _, n := range nodes {
		if seen[n.Depth] {
			continue
		}
		seen[n.Depth] = true
		if more := perDepth[n.Depth] - k; more > 0 {
			elided = append(elided, DepthElision{Depth: n.Depth, More: more})
		}
	}
	return kept, elided
}

// zeroCallerHint builds the #485 advisory for a symbol with no static callers.
// An exported function/method with zero `calls` edges is not necessarily dead:
// it may be reached only through an interface or reflection dispatch (the MCP
// SDK invokes tool handlers via the toolSurface interface, leaving no static
// edge). We name the interface(s) the receiver type implements when we can, so
// the reader knows where to look instead of assuming "safe to delete".
func zeroCallerHint(view *graphquery.View, targets []graphquery.Node) string {
	for _, t := range targets {
		if t.Kind != graph.NodeFunction && t.Kind != graph.NodeMethod {
			continue
		}
		if !isExportedName(t.Name) {
			continue
		}
		ifaces := implementedInterfaces(view, t.ID)
		if len(ifaces) > 0 {
			return fmt.Sprintf("0 static callers; %s satisfies interface(s) %s — likely invoked via interface/reflection dispatch (e.g. the MCP SDK). Check the interface implementors, not just static call edges.",
				t.Name, strings.Join(ifaces, ", "))
		}
		return "0 static callers, but this is an exported symbol — it may be invoked via interface/reflection dispatch (e.g. the MCP SDK), not a static call edge. This is not proof the symbol is dead."
	}
	return ""
}

// nameBasedEmptyHint returns a caveat WHEN an empty calls-edge result is for a
// target in a tree-sitter (name-based) language. Unlike Go's type-resolved
// edges, those extractors have incomplete recall, so an empty result is not
// proof that no callers/callees exist — the agent should verify with grep
// rather than conclude "none". Returns "" when every target is Go, so a Go
// empty falls through to the exact #485 interface-dispatch hint instead.
func nameBasedEmptyHint(targets []graphquery.Node, rel string) string {
	seen := map[string]bool{}
	var langs []string
	for _, t := range targets {
		if l := t.Language(); l != "go" && !seen[l] {
			seen[l] = true
			langs = append(langs, l)
		}
	}
	if len(langs) == 0 {
		return ""
	}
	sort.Strings(langs)
	return fmt.Sprintf("no %s edges found, but %s call-graph edges are name-based (tree-sitter) with incomplete recall — an empty result is not proof there are none. Verify with grep on the symbol name.",
		rel, strings.Join(langs, "/"))
}

func isExportedName(name string) bool {
	if name == "" {
		return false
	}
	r := rune(name[0])
	return r >= 'A' && r <= 'Z'
}

// implementedInterfaces returns the names of interfaces implemented by the type
// that owns the given method node. It walks: method <-has_method- type
// -implements-> interface. Returns nil for a non-method or an unattached type.
func implementedInterfaces(view *graphquery.View, methodID string) []string {
	var ifaces []string
	seen := map[string]bool{}
	for _, in := range view.EdgesByDst[methodID] {
		if in.Kind != graph.EdgeHasMethod {
			continue
		}
		typeID := in.SrcID
		for _, out := range view.EdgesBySrc[typeID] {
			if out.Kind != graph.EdgeImplements {
				continue
			}
			if n, ok := view.NodesByID[out.DstID]; ok && n.Name != "" && !seen[n.Name] {
				seen[n.Name] = true
				ifaces = append(ifaces, n.Name)
			}
		}
	}
	sort.Strings(ifaces)
	return ifaces
}
