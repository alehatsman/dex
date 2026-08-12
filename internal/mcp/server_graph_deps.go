package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/alehatsman/dex/internal/graph"
	"github.com/alehatsman/dex/internal/graph/resolve"
	"github.com/alehatsman/dex/internal/graphquery"
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
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project or git worktree you are working in. The server cannot see your shell's directory; when working in a worktree different from where the server started, pass that worktree's path"`
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
	p, hint := s.resolveProject(ctx, in.ProjectRoot)
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
		nodes := view.NodesByPath[in.Path]
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
	for _, n := range view.NodesByPackage[pkg] {
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
	for _, e := range view.EdgesBySrc[pkgID] {
		if e.Kind != graph.EdgeImports {
			continue
		}
		dst, ok := view.NodesByID[e.DstID]
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
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project or git worktree you are working in. The server cannot see your shell's directory; when working in a worktree different from where the server started, pass that worktree's path"`
	// Level selects the aggregation granularity: "module" (default) is the
	// per-file/per-Go-package DAG; "project" rolls JS/TS modules up to their
	// workspace package (@acme/ui → @acme/common), dropping intra-project
	// edges (#127 Phase 3).
	Level string `json:"level,omitempty" jsonschema:"aggregation level: 'module' (default) or 'project' (roll JS/TS modules up to workspace packages)"`
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
	p, hint := s.resolveProject(ctx, in.ProjectRoot)
	if hint != "" {
		return nil, PackageGraphOutput{Status: "error", Hint: hint}, nil
	}
	// Gate the project rollup on a ROOT-only workspace marker (#154/#162), before
	// any index work — the workspace-root fact is index-independent. resolve.Load
	// walks the whole tree for package.json, so on a non-workspace repo (e.g. a
	// Go module with buried JS/TS test fixtures) it would invent bogus workspace
	// projects from those fixtures. ProjectOfForRoot is the same gate + mapper the
	// ask(package_topology) path uses (projectOfFor, #151); it recognizes both a
	// JS/TS workspace and a Cargo workspace and hands back the matching mapper.
	projectOf, isWorkspace := resolve.ProjectOfForRoot(p.Root)
	if in.Level == "project" && !isWorkspace {
		return nil, PackageGraphOutput{Status: "no-graph", Project: p.Root,
			Hint: fmt.Sprintf("level=project needs a JS/TS workspace root (pnpm-workspace.yaml / rush.json / lerna.json / package.json \"workspaces\") or a Cargo workspace root (Cargo.toml [workspace]); %s is not one — use the default module level.", p.Root)}, nil
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

	var pg graphquery.PackageGraph
	if in.Level == "project" {
		pg = graphquery.BuildProjectGraph(view, projectOf)
	} else {
		pg = graphquery.BuildPackageGraph(view)
	}
	if len(pg.Nodes) == 0 {
		hint := "no package import graph — the module level covers Go, JS/TS, Python and Rust; a repo in another language returns no-graph. Try level=\"project\" for a JS/TS or Cargo workspace rollup."
		if in.Level == "project" {
			hint = "no workspace-project graph — no workspace packages/crates resolved, or no cross-project imports were indexed."
		}
		return nil, PackageGraphOutput{Status: "no-graph", Project: p.Root, Hint: hint}, nil
	}
	return nil, PackageGraphOutput{
		Status:  "ok",
		Project: p.Root,
		Nodes:   packageNodesFrom(pg.Nodes),
		Edges:   packageEdgesFrom(pg.Edges),
	}, nil
}
