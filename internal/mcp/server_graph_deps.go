package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/alehatsman/dex/internal/graph"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func (s *Server) GraphDeps(ctx context.Context, in GraphDepsInput) (GraphDepsOutput, error) {
	_, out, err := s.graphDeps(ctx, nil, in)
	return out, err
}

func (s *Server) PackageGraph(ctx context.Context, in PackageGraphInput) (PackageGraphOutput, error) {
	_, out, err := s.packageGraph(ctx, nil, in)
	return out, err
}

type GraphDepsInput struct {
	Path        string `json:"path,omitempty" jsonschema:"relative file path inside the project — resolved to its package"`
	Package     string `json:"package,omitempty" jsonschema:"full package path (e.g. 'github.com/foo/bar/internal/baz'); takes precedence over path"`
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
}

// GraphDep is a single import relationship: from_package depends on
// to_package. Layer 1 emits one edge per (importing-pkg, imported-pkg)
// pair, so the output is package-grained — not yet file-grained.
type GraphDep struct {
	FromPackage string `json:"from_package"`
	ToPackage   string `json:"to_package"`
}

type GraphDepsOutput struct {
	Status  string     `json:"status"` // "ok" | "no-index" | "no-graph" | "not-found" | "error"
	Hint    string     `json:"hint,omitempty"`
	Project string     `json:"project,omitempty"`
	Package string     `json:"package,omitempty"` // resolved package path the answer is for
	Imports []GraphDep `json:"imports,omitempty"`
}

func (s *Server) graphDeps(ctx context.Context, _ *sdk.CallToolRequest, in GraphDepsInput) (*sdk.CallToolResult, GraphDepsOutput, error) {
	if strings.TrimSpace(in.Path) == "" && strings.TrimSpace(in.Package) == "" {
		return nil, GraphDepsOutput{Status: "error", Hint: "pass `path` (a file inside the project) or `package` (full package path)"}, nil
	}
	p, hint := s.resolveProject(in.ProjectRoot)
	if hint != "" {
		return nil, GraphDepsOutput{Status: "error", Hint: hint}, nil
	}
	if _, err := os.Stat(p.DBPath); errors.Is(err, os.ErrNotExist) {
		return nil, GraphDepsOutput{Status: "no-index", Project: p.Root,
			Hint: fmt.Sprintf("no index for %s — run `dex index %s` first.", p.Root, p.Root)}, nil
	}
	st, err := s.openStore(p.DBPath)
	if err != nil {
		return nil, GraphDepsOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
	}

	view, err := s.cachedLoadGraphView(ctx, st, p.DBPath)
	if err != nil {
		return nil, GraphDepsOutput{Status: "error", Hint: fmt.Sprintf("load graph: %v", err)}, nil
	}
	if view == nil {
		return nil, GraphDepsOutput{Status: "no-graph", Project: p.Root,
			Hint: fmt.Sprintf("graph not indexed for %s — run `dex index %s --graph=only`.", p.Root, p.Root)}, nil
	}

	// Resolve target package. `package` wins over `path` when both are set.
	pkg := strings.TrimSpace(in.Package)
	if pkg == "" {
		// Path → package via nodesByPath. Multiple files share a pkg
		// (and the import graph is per-pkg anyway) so any node in that
		// file is fine.
		nodes := view.nodesByPath[in.Path]
		if len(nodes) == 0 {
			return nil, GraphDepsOutput{Status: "not-found", Project: p.Root,
				Hint: fmt.Sprintf("path %q has no graph nodes — file may be outside the indexed languages (currently Go-only) or not yet indexed", in.Path)}, nil
		}
		for _, n := range nodes {
			if n.PackagePath != "" {
				pkg = n.PackagePath
				break
			}
		}
		if pkg == "" {
			return nil, GraphDepsOutput{Status: "not-found", Project: p.Root,
				Hint: fmt.Sprintf("path %q resolved to no package — likely a file outside any package", in.Path)}, nil
		}
	}

	// Collect import edges where src is the NodePackage for pkg.
	// Layer 1 emits imports at the package node, so this is a single
	// edges-by-src lookup once we have the package node ID.
	var pkgID string
	for _, n := range view.nodesByPackage[pkg] {
		if n.Kind == graph.NodePackage {
			pkgID = n.ID
			break
		}
	}
	if pkgID == "" {
		return nil, GraphDepsOutput{Status: "not-found", Project: p.Root,
			Hint: fmt.Sprintf("package %q has no node in the graph", pkg)}, nil
	}

	out := GraphDepsOutput{Status: "ok", Project: p.Root, Package: pkg}
	for _, e := range view.edgesBySrc[pkgID] {
		if e.Kind != graph.EdgeImports {
			continue
		}
		dst, ok := view.nodesByID[e.DstID]
		if !ok || dst.Kind != graph.NodeImport {
			continue
		}
		out.Imports = append(out.Imports, GraphDep{
			FromPackage: pkg,
			ToPackage:   dst.QualifiedName, // import nodes carry the import path here
		})
	}
	sort.Slice(out.Imports, func(i, j int) bool { return out.Imports[i].ToPackage < out.Imports[j].ToPackage })
	return nil, out, nil
}

// ─── tool: graph_packages ─────────────────────────────────────────────────
//
// graph_deps answers "what does ONE package import"; graph_packages
// returns the WHOLE internal package import DAG in a single call, so a
// consumer (e.g. moongit's /explore "Map of the codebase") can rank and
// layer packages without N per-package round-trips.

type PackageGraphInput struct {
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
}

// PackageNode is one internal package in the import DAG.
//
// in_degree / out_degree / page_rank are derived from the package-level
// `imports` edges here — NOT read from graph_nodes' centrality columns,
// which the call-graph computation leaves zero on package nodes by
// design (see internal/graph/centrality.go). in_degree counts distinct
// internal packages that import this one (how load-bearing it is);
// out_degree counts the distinct internal packages it imports.
type PackageNode struct {
	Package   string  `json:"package"`
	InDegree  int     `json:"in_degree"`
	OutDegree int     `json:"out_degree"`
	PageRank  float64 `json:"page_rank"`
	// IsMain marks an executable entry point: the package's clause name is
	// "main" (Go `package main`). A reliable entry-point signal where
	// in_degree==0 is not — a helper imported only by _test.go files also
	// has in_degree 0 here (test imports are excluded from this DAG) yet is
	// no entry point.
	IsMain bool `json:"is_main"`
}

// PackageEdge is one internal import relationship: FromPackage imports
// ToPackage. External (stdlib / third-party) imports are excluded —
// both endpoints have their own package node in the project.
type PackageEdge struct {
	FromPackage string `json:"from_package"`
	ToPackage   string `json:"to_package"`
}

type PackageGraphOutput struct {
	Status  string        `json:"status"` // "ok" | "no-index" | "no-graph" | "error"
	Hint    string        `json:"hint,omitempty"`
	Project string        `json:"project,omitempty"`
	Nodes   []PackageNode `json:"nodes,omitempty"`
	Edges   []PackageEdge `json:"edges,omitempty"`
}

func (s *Server) packageGraph(ctx context.Context, _ *sdk.CallToolRequest, in PackageGraphInput) (*sdk.CallToolResult, PackageGraphOutput, error) {
	p, hint := s.resolveProject(in.ProjectRoot)
	if hint != "" {
		return nil, PackageGraphOutput{Status: "error", Hint: hint}, nil
	}
	if _, err := os.Stat(p.DBPath); errors.Is(err, os.ErrNotExist) {
		return nil, PackageGraphOutput{Status: "no-index", Project: p.Root,
			Hint: fmt.Sprintf("no index for %s — run `dex index %s` first.", p.Root, p.Root)}, nil
	}
	st, err := s.openStore(p.DBPath)
	if err != nil {
		return nil, PackageGraphOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
	}

	view, err := s.cachedLoadGraphView(ctx, st, p.DBPath)
	if err != nil {
		return nil, PackageGraphOutput{Status: "error", Hint: fmt.Sprintf("load graph: %v", err)}, nil
	}
	if view == nil {
		return nil, PackageGraphOutput{Status: "no-graph", Project: p.Root,
			Hint: fmt.Sprintf("graph not indexed for %s — run `dex index %s --graph=only`.", p.Root, p.Root)}, nil
	}

	out := buildPackageGraph(view)
	out.Project = p.Root
	return nil, out, nil
}

// buildPackageGraph derives the internal package import DAG from a
// loaded graphView. Pure (no I/O) so it unit-tests against a hand-built
// view. The import graph lives on EdgeImports edges: src is the
// importing package's NodePackage; dst is a NodeImport whose
// QualifiedName is the imported path. An import is "internal" when that
// path has its own NodePackage in the project — external imports
// (stdlib / third-party) have no package node and are dropped.
// isGoPackageNode reports whether n is a Go package node. The package import
// DAG is Go-only: the Go extractor emits package nodes with no "language" in
// their metadata, while every tree-sitter extractor stamps its package nodes
// with Metadata["language"] (sitter_javascript.go etc.). So a NodePackage
// without a "language" key is Go; one with it is a non-Go package (web/src TS,
// python/rust/js testdata fixtures) that has no place in this DAG. Nodes carry
// no metadata at all (the common Go case) → Go.
func isGoPackageNode(n graphNode) bool {
	if n.Kind != graph.NodePackage {
		return false
	}
	if len(n.MetadataJSON) == 0 {
		return true
	}
	var md map[string]any
	if err := json.Unmarshal(n.MetadataJSON, &md); err != nil {
		return true // unparseable metadata: don't exclude a real Go package
	}
	_, nonGo := md["language"]
	return !nonGo
}

func buildPackageGraph(view *graphView) PackageGraphOutput {
	internal := map[string]struct{}{}
	mainByPath := map[string]bool{} // package clause name == "main" → executable
	for _, n := range view.nodesByID {
		if isGoPackageNode(n) && n.PackagePath != "" {
			internal[n.PackagePath] = struct{}{}
			if n.Name == "main" {
				mainByPath[n.PackagePath] = true
			}
		}
	}
	if len(internal) == 0 {
		return PackageGraphOutput{Status: "no-graph",
			Hint: "no Go package import graph — this endpoint is Go-only today; non-Go repos return no-graph."}
	}

	// Dedup edges on (from, to); build degree counts and the
	// out-adjacency for PageRank in one pass.
	type pair struct{ from, to string }
	seen := map[pair]struct{}{}
	inDeg := map[string]int{}
	outDeg := map[string]int{}
	outAdj := map[string]map[string]struct{}{}
	var edges []PackageEdge
	for _, e := range view.edgesByKind[graph.EdgeImports] {
		src, ok := view.nodesByID[e.SrcID]
		if !ok || src.Kind != graph.NodePackage {
			continue
		}
		dst, ok := view.nodesByID[e.DstID]
		if !ok || dst.Kind != graph.NodeImport {
			continue
		}
		from, to := src.PackagePath, dst.QualifiedName // import node carries the imported path
		if from == "" || to == "" || from == to {
			continue
		}
		// Both endpoints must be Go packages in this project. `internal` is
		// already the Go-package set, so this drops external imports and any
		// edge touching a non-Go (tree-sitter) package node.
		if _, ok := internal[from]; !ok {
			continue
		}
		if _, ok := internal[to]; !ok {
			continue
		}
		if _, dup := seen[pair{from, to}]; dup {
			continue
		}
		seen[pair{from, to}] = struct{}{}
		edges = append(edges, PackageEdge{FromPackage: from, ToPackage: to})
		outDeg[from]++
		inDeg[to]++
		if outAdj[from] == nil {
			outAdj[from] = map[string]struct{}{}
		}
		outAdj[from][to] = struct{}{}
	}

	// PageRank over the import DAG. Rank flows importer → imported, so
	// foundation packages many others depend on accumulate weight —
	// the "load-bearing core floats up" signal even when raw in-degree
	// is modest. Keyed by package path (the id space used in outAdj).
	ids := make([]string, 0, len(internal))
	for pkg := range internal {
		ids = append(ids, pkg)
	}
	ranks := graph.PageRank(ids, outAdj)

	nodes := make([]PackageNode, 0, len(internal))
	for pkg := range internal {
		nodes = append(nodes, PackageNode{
			Package:   pkg,
			InDegree:  inDeg[pkg],
			OutDegree: outDeg[pkg],
			PageRank:  ranks[pkg],
			IsMain:    mainByPath[pkg],
		})
	}

	// Deterministic output: nodes by in-degree desc then path; edges by
	// (from, to). Keeps responses and golden tests stable.
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].InDegree != nodes[j].InDegree {
			return nodes[i].InDegree > nodes[j].InDegree
		}
		return nodes[i].Package < nodes[j].Package
	})
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].FromPackage != edges[j].FromPackage {
			return edges[i].FromPackage < edges[j].FromPackage
		}
		return edges[i].ToPackage < edges[j].ToPackage
	})

	return PackageGraphOutput{Status: "ok", Nodes: nodes, Edges: edges}
}
