package mcp

// server_graph.go holds the graph-only MCP tools: graph_deps,
// graph_callers, graph_callees. Each handler reads the static graph
// (graph_nodes / graph_edges) via loadGraphView and never touches the
// embedding or chat endpoints — making these the cheapest tools in
// the surface and useful as a precise fallback when semantic search
// drifts.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/alehatsman/dex/internal/graph"
	"github.com/alehatsman/dex/internal/store"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// GraphDeps, GraphCallers, GraphCallees are exported wrappers around
// the unexported MCP handlers so the CLI can reuse them. Mirrors the
// Search/FindSymbol/... wrappers in server.go.
func (s *Server) GraphDeps(ctx context.Context, in GraphDepsInput) (GraphDepsOutput, error) {
	_, out, err := s.graphDeps(ctx, nil, in)
	return out, err
}

func (s *Server) GraphCallers(ctx context.Context, in CallEdgeInput) (CallEdgeOutput, error) {
	_, out, err := s.graphCallers(ctx, nil, in)
	return out, err
}

func (s *Server) GraphCallees(ctx context.Context, in CallEdgeInput) (CallEdgeOutput, error) {
	_, out, err := s.graphCallees(ctx, nil, in)
	return out, err
}

func (s *Server) PackageGraph(ctx context.Context, in PackageGraphInput) (PackageGraphOutput, error) {
	_, out, err := s.packageGraph(ctx, nil, in)
	return out, err
}

// ─── tool: graph_deps ─────────────────────────────────────────────────────

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
	st, err := store.OpenWith(ctx, p.DBPath, s.StoreOpts)
	if err != nil {
		return nil, GraphDepsOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
	}
	defer func() { _ = st.Close() }()

	view, err := loadGraphView(ctx, st)
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
	st, err := store.OpenWith(ctx, p.DBPath, s.StoreOpts)
	if err != nil {
		return nil, PackageGraphOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
	}
	defer func() { _ = st.Close() }()

	view, err := loadGraphView(ctx, st)
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

// ─── tools: graph_callers / graph_callees ─────────────────────────────────

type CallEdgeInput struct {
	Name        string `json:"name" jsonschema:"symbol to query: bare ('Foo'), receiver-qualified ('(*Server).RunStdio'), or package-tail-qualified ('mcp.NewServer')"`
	Package     string `json:"package,omitempty" jsonschema:"optional package path filter when the same name is defined in multiple packages"`
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
	K           int    `json:"k,omitempty" jsonschema:"max hits to return (default 12, max 50)"`
}

// CallSite is one calls-edge endpoint — the function on the other end
// of the edge, plus the file:line where the call expression sits.
type CallSite struct {
	QualifiedName string `json:"qualified_name"`
	Package       string `json:"package,omitempty"`
	Kind          string `json:"kind"` // "function" | "method" | "interface_method"
	Path          string `json:"path"`
	StartLine     int    `json:"start_line"`
	EndLine       int    `json:"end_line"`
	CallSitePath  string `json:"call_site_path,omitempty"` // file containing the call expression
	CallSiteLine  int    `json:"call_site_line,omitempty"` // line of the call expression
	// Role tags the peer the same way SearchHit.Role does: how this
	// function sits in the call graph. Empty for unremarkable peers.
	// See formatRole for the threshold/tiering rules.
	Role      string `json:"role,omitempty"`
	Content   string `json:"content,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// TargetMatch is one resolved interpretation of the input `name`.
// Returned even when there's no calls activity, so the caller can
// disambiguate or confirm the resolution.
type TargetMatch struct {
	QualifiedName string `json:"qualified_name"`
	Package       string `json:"package,omitempty"`
	Kind          string `json:"kind"`
	Path          string `json:"path,omitempty"`
	StartLine     int    `json:"start_line,omitempty"`
}

type CallEdgeOutput struct {
	Status  string        `json:"status"` // "ok" | "no-index" | "no-graph" | "not-found" | "error"
	Hint    string        `json:"hint,omitempty"`
	Project string        `json:"project,omitempty"`
	Targets []TargetMatch `json:"targets,omitempty"`
	Hits    []CallSite    `json:"hits,omitempty"`
}

func (s *Server) graphCallers(ctx context.Context, _ *sdk.CallToolRequest, in CallEdgeInput) (*sdk.CallToolResult, CallEdgeOutput, error) {
	return s.callEdges(ctx, in, true)
}

func (s *Server) graphCallees(ctx context.Context, _ *sdk.CallToolRequest, in CallEdgeInput) (*sdk.CallToolResult, CallEdgeOutput, error) {
	return s.callEdges(ctx, in, false)
}

// callEdges is the shared body. callers=true walks edgesByDst (incoming
// calls); callers=false walks edgesBySrc (outgoing calls).
func (s *Server) callEdges(ctx context.Context, in CallEdgeInput, callers bool) (*sdk.CallToolResult, CallEdgeOutput, error) {
	if strings.TrimSpace(in.Name) == "" {
		return nil, CallEdgeOutput{Status: "error", Hint: "name is empty"}, nil
	}
	p, hint := s.resolveProject(in.ProjectRoot)
	if hint != "" {
		return nil, CallEdgeOutput{Status: "error", Hint: hint}, nil
	}
	if _, err := os.Stat(p.DBPath); errors.Is(err, os.ErrNotExist) {
		return nil, CallEdgeOutput{Status: "no-index", Project: p.Root,
			Hint: fmt.Sprintf("no index for %s — run `dex index %s` first.", p.Root, p.Root)}, nil
	}
	st, err := store.OpenWith(ctx, p.DBPath, s.StoreOpts)
	if err != nil {
		return nil, CallEdgeOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
	}
	defer func() { _ = st.Close() }()

	view, err := loadGraphView(ctx, st)
	if err != nil {
		return nil, CallEdgeOutput{Status: "error", Hint: fmt.Sprintf("load graph: %v", err)}, nil
	}
	if view == nil {
		return nil, CallEdgeOutput{Status: "no-graph", Project: p.Root,
			Hint: fmt.Sprintf("graph not indexed for %s — run `dex index %s --graph=only`.", p.Root, p.Root)}, nil
	}
	if len(view.edgesByKind[graph.EdgeCalls]) == 0 {
		return nil, CallEdgeOutput{Status: "no-graph", Project: p.Root,
			Hint: "graph has no `calls` edges — reindex the project with this release (`dex index . --graph=only`) to extract them."}, nil
	}

	targets := resolveCallTargets(view, in.Name, in.Package)
	if len(targets) == 0 {
		return nil, CallEdgeOutput{Status: "not-found", Project: p.Root,
			Hint: fmt.Sprintf("no graph node matches name=%q — try the bare identifier or the receiver-qualified form like '(*Type).Method'", in.Name)}, nil
	}

	k := in.K
	if k <= 0 {
		k = 12
	}
	if k > 50 {
		k = 50
	}

	out := CallEdgeOutput{Status: "ok", Project: p.Root}
	for _, t := range targets {
		out.Targets = append(out.Targets, TargetMatch{
			QualifiedName: t.QualifiedName,
			Package:       t.PackagePath,
			Kind:          string(t.Kind),
			Path:          t.FilePath,
			StartLine:     t.StartLine,
		})
	}

	seen := map[string]bool{}
	for _, t := range targets {
		var edges []graphEdge
		if callers {
			edges = view.edgesByDst[t.ID]
		} else {
			edges = view.edgesBySrc[t.ID]
		}
		for _, e := range edges {
			if e.Kind != graph.EdgeCalls {
				continue
			}
			peerID := e.SrcID
			if !callers {
				peerID = e.DstID
			}
			peer, ok := view.nodesByID[peerID]
			if !ok {
				continue
			}
			// Dedup on (peer node id, call-site file+line). Different
			// call sites from the same caller are distinct hits.
			key := peer.ID + "@" + e.FilePath + ":" + fmt.Sprint(e.StartLine)
			if seen[key] {
				continue
			}
			seen[key] = true
			hit := CallSite{
				QualifiedName: peer.QualifiedName,
				Package:       peer.PackagePath,
				Kind:          string(peer.Kind),
				Path:          peer.FilePath,
				StartLine:     peer.StartLine,
				EndLine:       peer.EndLine,
				CallSitePath:  e.FilePath,
				CallSiteLine:  e.StartLine,
				Role:          formatRole(peer.Name, peer.InDegree, peer.OutDegree, peer.CrossPkgCallers, peer.Betweenness),
			}
			out.Hits = append(out.Hits, hit)
		}
	}

	// Sort hits by peer centrality, then by path/line for determinism.
	// peerCentrality is a closure over view.nodesByID so we don't
	// re-resolve per hit. PageRank dominates; in_degree breaks ties
	// for peers that didn't pick up rank (e.g. callees with no
	// incoming edges in the indexed slice).
	peerCentrality := func(h CallSite) (float64, int) {
		// Resolve peer node by qualified name + package — the same key
		// we used when populating the hit.
		for _, n := range view.nodesByQualified[h.QualifiedName] {
			if n.PackagePath == h.Package {
				return n.PageRank, n.InDegree
			}
		}
		for _, n := range view.nodesByName[h.QualifiedName] {
			if n.PackagePath == h.Package {
				return n.PageRank, n.InDegree
			}
		}
		return 0, 0
	}
	sort.SliceStable(out.Hits, func(i, j int) bool {
		ai, aj := out.Hits[i], out.Hits[j]
		pi, di := peerCentrality(ai)
		pj, dj := peerCentrality(aj)
		if pi != pj {
			return pi > pj
		}
		if di != dj {
			return di > dj
		}
		if ai.Path != aj.Path {
			return ai.Path < aj.Path
		}
		if ai.StartLine != aj.StartLine {
			return ai.StartLine < aj.StartLine
		}
		return ai.CallSiteLine < aj.CallSiteLine
	})

	// Cap to the k most-central hits. The truncation is applied AFTER the
	// centrality sort (not during edge iteration) so we return the true
	// top-k by centrality rather than whichever peers the graph-edge
	// traversal happened to visit first.
	if len(out.Hits) > k {
		out.Hits = out.Hits[:k]
	}

	// Inline a short slice of each hit's containing function so the
	// agent doesn't need a follow-up Read for context. Same shape as
	// inlineContent's per-read budget for targeted intents.
	const (
		maxHitLines = 30
		maxHitBytes = 2 * 1024
	)
	for i := range out.Hits {
		abs := out.Hits[i].Path
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(p.Root, abs)
		}
		content, truncated, err := readLineRange(abs, out.Hits[i].StartLine, out.Hits[i].EndLine, maxHitLines, maxHitBytes)
		if err == nil {
			out.Hits[i].Content = content
			out.Hits[i].Truncated = truncated
		}
	}

	return nil, out, nil
}

// resolveCallTargets maps the user-supplied `name` (and optional pkg
// filter) onto graph nodes. Recognised shapes, in order:
//
//	"Foo"                  — bare; matches NodeFunction / NodeMethod / NodeType by Name
//	"(*T).Foo" / "T.Foo"   — receiver-qualified; matches by QualifiedName
//	"pkg.Foo"              — package-tail-qualified; PackagePath must end with /pkg or equal pkg
//
// Multiple matches are returned so the caller can disambiguate. The
// optional `pkgFilter` collapses ambiguity by full package path.
func resolveCallTargets(view *graphView, name, pkgFilter string) []graphNode {
	name = strings.TrimSpace(name)
	pkgFilter = strings.TrimSpace(pkgFilter)
	if name == "" {
		return nil
	}
	want := func(n graphNode) bool {
		switch n.Kind {
		case graph.NodeFunction, graph.NodeMethod:
			return true
		default:
			return false
		}
	}
	pkgOK := func(n graphNode) bool {
		if pkgFilter == "" {
			return true
		}
		return n.PackagePath == pkgFilter
	}
	out := []graphNode{}
	seen := map[string]bool{}
	add := func(n graphNode) {
		if seen[n.ID] || !want(n) || !pkgOK(n) {
			return
		}
		seen[n.ID] = true
		out = append(out, n)
	}

	// 1) Exact QualifiedName match — covers "(*T).Foo", "T.Foo", and
	//    bare function names that happen to be unique within a pkg.
	for _, n := range view.nodesByQualified[name] {
		add(n)
	}
	// 2) Bare Name match — covers "Foo" both as a function name and
	//    as the method portion of "(*T).Foo" (graph stores Name="Foo"
	//    alongside QualifiedName="(*T).Foo").
	for _, n := range view.nodesByName[name] {
		add(n)
	}
	if len(out) > 0 {
		return out
	}

	// 3) "pkg.Foo" — split on the last dot and try pkg-tail matching.
	//    Only attempt when there's exactly one dot and the second
	//    segment looks like an identifier (no receiver parens).
	if i := strings.LastIndex(name, "."); i > 0 && !strings.ContainsAny(name, "()*") {
		pkgTail, bare := name[:i], name[i+1:]
		for _, n := range view.nodesByName[bare] {
			tail := n.PackagePath
			if j := strings.LastIndex(tail, "/"); j >= 0 {
				tail = tail[j+1:]
			}
			if tail == pkgTail {
				add(n)
			}
		}
	}
	return out
}

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
	st, err := store.OpenWith(ctx, p.DBPath, s.StoreOpts)
	if err != nil {
		return nil, ImpactOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
	}
	defer func() { _ = st.Close() }()

	view, err := loadGraphView(ctx, st)
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

// ─── tools: graph_links / graph_backlinks ─────────────────────────────────

type DocLinkInput struct {
	Doc         string `json:"doc" jsonschema:"path to a markdown document relative to the project root (e.g. 'docs/spec.md'); a bare basename like 'spec' is also accepted when unambiguous"`
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
	K           int    `json:"k,omitempty" jsonschema:"max hits to return (default 50, max 200)"`
}

// DocLink is one endpoint of a doc-graph edge: the document on the other
// end, plus the file:line of the link expression that produced the edge.
type DocLink struct {
	Doc          string `json:"doc"`  // relpath of the peer document (its parent doc when the peer is a heading)
	Name         string `json:"name"` // basename of the peer document, or the heading text
	Kind         string `json:"kind"` // "links" | "wikilinks" | "transcludes"
	LinkSitePath string `json:"link_site_path,omitempty"`
	LinkSiteLine int    `json:"link_site_line,omitempty"`
	// TargetAnchor is the heading slug the reference points at, when the
	// edge targets a section rather than a whole document. For backlinks
	// it names the section of the queried doc that was linked.
	TargetAnchor string `json:"target_anchor,omitempty"`
}

// DocTarget is a resolved interpretation of the input `doc`. Returned
// even with no links so the caller can confirm the resolution.
type DocTarget struct {
	Doc  string `json:"doc"`
	Name string `json:"name"`
}

type DocLinkOutput struct {
	Status  string      `json:"status"` // "ok" | "no-index" | "no-graph" | "not-found" | "error"
	Hint    string      `json:"hint,omitempty"`
	Project string      `json:"project,omitempty"`
	Targets []DocTarget `json:"targets,omitempty"`
	Hits    []DocLink   `json:"hits,omitempty"`
}

// GraphLinks, GraphBacklinks are exported wrappers so the CLI can reuse
// the handlers, mirroring GraphCallers/GraphCallees.
func (s *Server) GraphLinks(ctx context.Context, in DocLinkInput) (DocLinkOutput, error) {
	_, out, err := s.graphLinks(ctx, nil, in)
	return out, err
}

func (s *Server) GraphBacklinks(ctx context.Context, in DocLinkInput) (DocLinkOutput, error) {
	_, out, err := s.graphBacklinks(ctx, nil, in)
	return out, err
}

func (s *Server) graphLinks(ctx context.Context, _ *sdk.CallToolRequest, in DocLinkInput) (*sdk.CallToolResult, DocLinkOutput, error) {
	return s.docEdges(ctx, in, false)
}

func (s *Server) graphBacklinks(ctx context.Context, _ *sdk.CallToolRequest, in DocLinkInput) (*sdk.CallToolResult, DocLinkOutput, error) {
	return s.docEdges(ctx, in, true)
}

// docEdges is the shared body for the doc-graph verbs. backlinks=false
// (graph_links) walks edgesBySrc — documents this doc points to;
// backlinks=true (graph_backlinks) walks edgesByDst — documents that
// point here. Both keep only `links`/`wikilinks` edges, so it never
// surfaces code (`calls`/`imports`) edges even on a mixed node id.
func (s *Server) docEdges(ctx context.Context, in DocLinkInput, backlinks bool) (*sdk.CallToolResult, DocLinkOutput, error) {
	if strings.TrimSpace(in.Doc) == "" {
		return nil, DocLinkOutput{Status: "error", Hint: "doc is empty"}, nil
	}
	p, hint := s.resolveProject(in.ProjectRoot)
	if hint != "" {
		return nil, DocLinkOutput{Status: "error", Hint: hint}, nil
	}
	if _, err := os.Stat(p.DBPath); errors.Is(err, os.ErrNotExist) {
		return nil, DocLinkOutput{Status: "no-index", Project: p.Root,
			Hint: fmt.Sprintf("no index for %s — run `dex index %s` first.", p.Root, p.Root)}, nil
	}
	st, err := store.OpenWith(ctx, p.DBPath, s.StoreOpts)
	if err != nil {
		return nil, DocLinkOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
	}
	defer func() { _ = st.Close() }()

	view, err := loadGraphView(ctx, st)
	if err != nil {
		return nil, DocLinkOutput{Status: "error", Hint: fmt.Sprintf("load graph: %v", err)}, nil
	}
	if view == nil {
		return nil, DocLinkOutput{Status: "no-graph", Project: p.Root,
			Hint: fmt.Sprintf("graph not indexed for %s — run `dex index %s --graph=only`.", p.Root, p.Root)}, nil
	}

	targets := resolveDocTargets(view, in.Doc)
	if len(targets) == 0 {
		// Distinguish "no docs at all" (needs reindex with this release)
		// from "that path isn't a known doc" (likely a typo).
		hint := fmt.Sprintf("no document node matches %q — pass a path relative to the project root, e.g. 'docs/spec.md'.", in.Doc)
		if len(view.docNodes()) == 0 {
			hint = "graph has no document nodes — reindex with this release (`dex index . --graph=only`) to extract the markdown doc graph."
		}
		return nil, DocLinkOutput{Status: "not-found", Project: p.Root, Hint: hint}, nil
	}

	k := in.K
	if k <= 0 {
		k = 50
	}
	if k > 200 {
		k = 200
	}

	out := DocLinkOutput{Status: "ok", Project: p.Root}
	for _, t := range targets {
		out.Targets = append(out.Targets, DocTarget{Doc: t.QualifiedName, Name: t.Name})
	}
	out.Hits = collectDocEdges(view, targets, backlinks, k)
	return nil, out, nil
}

// collectDocEdges walks the doc-graph edges incident to targets and
// returns the peer documents, keeping only `links`/`wikilinks`/
// `transcludes` edges so code edges (`calls`/`imports`/…) never leak in.
// backlinks=false walks outgoing edges (docs the target points to);
// backlinks=true walks incoming edges (docs that point to the target),
// rolled up across the doc's heading nodes so a link to `doc.md#section`
// still counts as a backlink of `doc.md`. When an edge targets a heading,
// TargetAnchor names the section and Doc resolves to the parent document.
// Hits are deduped, sorted deterministically, and capped at k. Pure over
// view — unit-testable off a hand-built graph.
func collectDocEdges(view *graphView, targets []graphNode, backlinks bool, k int) []DocLink {
	seen := map[string]bool{}
	var hits []DocLink
	for _, t := range targets {
		// For backlinks, scan edges incident to the doc AND each of its
		// heading nodes (same FilePath, kind heading), so section-targeted
		// links roll up to the document. Outgoing links always originate
		// from the document node, so no expansion is needed there.
		endpoints := []string{t.ID}
		if backlinks {
			for _, n := range view.nodesByPath[t.FilePath] {
				if n.Kind == graph.NodeHeading {
					endpoints = append(endpoints, n.ID)
				}
			}
		}
		for _, id := range endpoints {
			edges := view.edgesBySrc[id]
			if backlinks {
				edges = view.edgesByDst[id]
			}
			for _, e := range edges {
				if e.Kind != graph.EdgeLinks && e.Kind != graph.EdgeWikilinks && e.Kind != graph.EdgeTransclude {
					continue
				}
				peerID := e.DstID
				if backlinks {
					peerID = e.SrcID
				}
				peer, ok := view.nodesByID[peerID]
				if !ok {
					continue
				}
				// Render the peer: a heading peer surfaces under its parent
				// doc with its text as the name.
				doc, name := peer.QualifiedName, peer.Name
				if peer.Kind == graph.NodeHeading {
					doc = peer.FilePath
				}
				// The link's destination names the section, if any — true in
				// both directions since edges always point src → (doc|heading).
				var anchor string
				if dn, ok := view.nodesByID[e.DstID]; ok && dn.Kind == graph.NodeHeading {
					anchor = anchorOf(dn.QualifiedName)
				}
				key := peerID + "|" + string(e.Kind) + "|" + e.FilePath + ":" + fmt.Sprint(e.StartLine) + "|" + anchor
				if seen[key] {
					continue
				}
				seen[key] = true
				hits = append(hits, DocLink{
					Doc:          doc,
					Name:         name,
					Kind:         string(e.Kind),
					LinkSitePath: e.FilePath,
					LinkSiteLine: e.StartLine,
					TargetAnchor: anchor,
				})
			}
		}
	}
	// Order by peer importance (doc-graph PageRank, then backlink in-degree)
	// so the most-referenced docs surface first, with path/kind/line/anchor
	// as deterministic tiebreakers. peerRank reads the centrality persisted
	// on the peer's document node; a heading peer borrows its parent doc's
	// rank.
	peerRank := func(h DocLink) (float64, int) {
		for _, n := range view.nodesByPath[h.Doc] {
			if n.Kind == graph.NodeDocument {
				return n.PageRank, n.InDegree
			}
		}
		return 0, 0
	}
	sort.SliceStable(hits, func(i, j int) bool {
		a, b := hits[i], hits[j]
		pa, da := peerRank(a)
		pb, db := peerRank(b)
		if pa != pb {
			return pa > pb
		}
		if da != db {
			return da > db
		}
		if a.Doc != b.Doc {
			return a.Doc < b.Doc
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.LinkSiteLine != b.LinkSiteLine {
			return a.LinkSiteLine < b.LinkSiteLine
		}
		return a.TargetAnchor < b.TargetAnchor
	})
	if len(hits) > k {
		hits = hits[:k]
	}
	return hits
}

// anchorOf returns the slug part of a heading QualifiedName ("doc.md#sec"
// → "sec"); "" when there's no fragment.
func anchorOf(qualifiedName string) string {
	if i := strings.Index(qualifiedName, "#"); i >= 0 {
		return qualifiedName[i+1:]
	}
	return ""
}

// ─── tool: graph_tags ─────────────────────────────────────────────────────

type TagInput struct {
	Tag         string `json:"tag,omitempty" jsonschema:"a markdown #tag (without the leading #) — returns the documents carrying it"`
	Doc         string `json:"doc,omitempty" jsonschema:"a document path — returns the tags that document carries; ignored when tag is set"`
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
	K           int    `json:"k,omitempty" jsonschema:"max items to return (default 100, max 500)"`
}

type TagOutput struct {
	Status  string   `json:"status"`            // "ok" | "no-index" | "no-graph" | "not-found" | "error"
	Hint    string   `json:"hint,omitempty"`    //
	Project string   `json:"project,omitempty"` //
	Query   string   `json:"query,omitempty"`   // the resolved tag or doc
	Result  string   `json:"result,omitempty"`  // "documents" (tag→docs) or "tags" (doc→tags)
	Items   []string `json:"items,omitempty"`   // doc relpaths, or tag names
}

func (s *Server) GraphTags(ctx context.Context, in TagInput) (TagOutput, error) {
	_, out, err := s.graphTags(ctx, nil, in)
	return out, err
}

// graphTags answers the two tag-clustering questions over the doc graph's
// `tagged` edges: `tag` → the documents carrying it; `doc` → the tags it
// carries. Exactly one of tag/doc is used (tag wins if both are set).
func (s *Server) graphTags(ctx context.Context, _ *sdk.CallToolRequest, in TagInput) (*sdk.CallToolResult, TagOutput, error) {
	tag := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(in.Tag), "#"))
	doc := strings.TrimSpace(in.Doc)
	if tag == "" && doc == "" {
		return nil, TagOutput{Status: "error", Hint: "pass `tag` (→ documents) or `doc` (→ tags)"}, nil
	}
	p, hint := s.resolveProject(in.ProjectRoot)
	if hint != "" {
		return nil, TagOutput{Status: "error", Hint: hint}, nil
	}
	if _, err := os.Stat(p.DBPath); errors.Is(err, os.ErrNotExist) {
		return nil, TagOutput{Status: "no-index", Project: p.Root,
			Hint: fmt.Sprintf("no index for %s — run `dex index %s` first.", p.Root, p.Root)}, nil
	}
	st, err := store.OpenWith(ctx, p.DBPath, s.StoreOpts)
	if err != nil {
		return nil, TagOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
	}
	defer func() { _ = st.Close() }()

	view, err := loadGraphView(ctx, st)
	if err != nil {
		return nil, TagOutput{Status: "error", Hint: fmt.Sprintf("load graph: %v", err)}, nil
	}
	if view == nil {
		return nil, TagOutput{Status: "no-graph", Project: p.Root,
			Hint: fmt.Sprintf("graph not indexed for %s — run `dex index %s --graph=only`.", p.Root, p.Root)}, nil
	}

	k := in.K
	if k <= 0 {
		k = 100
	}
	if k > 500 {
		k = 500
	}

	if tag != "" {
		// tag → documents. Find the tag node by name, walk its incoming
		// `tagged` edges, collect the source documents.
		var tagNode *graphNode
		for _, n := range view.nodesByName[tag] {
			if n.Kind == graph.NodeTag {
				nn := n
				tagNode = &nn
				break
			}
		}
		if tagNode == nil {
			return nil, TagOutput{Status: "not-found", Project: p.Root, Query: tag,
				Hint: fmt.Sprintf("no #%s tag in the doc graph.", tag)}, nil
		}
		docs := docsForTag(view, tagNode.ID)
		sortByDocCentrality(view, docs)
		if len(docs) > k {
			docs = docs[:k]
		}
		return nil, TagOutput{Status: "ok", Project: p.Root, Query: tag, Result: "documents", Items: docs}, nil
	}

	// doc → tags.
	targets := resolveDocTargets(view, doc)
	if len(targets) == 0 {
		return nil, TagOutput{Status: "not-found", Project: p.Root, Query: doc,
			Hint: fmt.Sprintf("no document node matches %q.", doc)}, nil
	}
	seen := map[string]bool{}
	var tags []string
	for _, t := range targets {
		for _, name := range tagsForDoc(view, t.ID) {
			if !seen[name] {
				seen[name] = true
				tags = append(tags, name)
			}
		}
	}
	sort.Strings(tags)
	if len(tags) > k {
		tags = tags[:k]
	}
	return nil, TagOutput{Status: "ok", Project: p.Root, Query: targets[0].QualifiedName, Result: "tags", Items: tags}, nil
}

// docsForTag returns the distinct documents carrying the tag node, via
// its incoming `tagged` edges. Pure over view — unit-testable.
func docsForTag(view *graphView, tagID string) []string {
	seen := map[string]bool{}
	var docs []string
	for _, e := range view.edgesByDst[tagID] {
		if e.Kind != graph.EdgeTagged {
			continue
		}
		if src, ok := view.nodesByID[e.SrcID]; ok && !seen[src.QualifiedName] {
			seen[src.QualifiedName] = true
			docs = append(docs, src.QualifiedName)
		}
	}
	return docs
}

// tagsForDoc returns the tag names a document carries, via its outgoing
// `tagged` edges. Pure over view — unit-testable.
func tagsForDoc(view *graphView, docID string) []string {
	var tags []string
	for _, e := range view.edgesBySrc[docID] {
		if e.Kind != graph.EdgeTagged {
			continue
		}
		if dst, ok := view.nodesByID[e.DstID]; ok {
			tags = append(tags, dst.Name)
		}
	}
	return tags
}

// sortByDocCentrality orders doc relpaths by their node's PageRank then
// in-degree (most-referenced first), with path as the deterministic
// tiebreaker. Docs absent from the view sort last.
func sortByDocCentrality(view *graphView, docs []string) {
	rank := func(rel string) (float64, int) {
		for _, n := range view.nodesByPath[rel] {
			if n.Kind == graph.NodeDocument {
				return n.PageRank, n.InDegree
			}
		}
		return 0, 0
	}
	sort.SliceStable(docs, func(i, j int) bool {
		pi, di := rank(docs[i])
		pj, dj := rank(docs[j])
		if pi != pj {
			return pi > pj
		}
		if di != dj {
			return di > dj
		}
		return docs[i] < docs[j]
	})
}

// resolveDocTargets maps the input `doc` onto document nodes. It tries an
// exact relpath match first (the qualified name of a doc node is its
// relpath), then falls back to a unique basename match for convenience.
func resolveDocTargets(view *graphView, doc string) []graphNode {
	doc = strings.TrimSpace(doc)
	doc = strings.TrimPrefix(filepath.ToSlash(doc), "./")
	if doc == "" {
		return nil
	}
	isDoc := func(n graphNode) bool { return n.Kind == graph.NodeDocument }

	// 1) Exact relpath match, with/without an extension. Resolve via
	//    nodesByPath (keyed on FilePath) — NOT nodesByQualified, which
	//    loadGraphView populates only when QualifiedName != Name, so a
	//    root-level doc like "README.md" (where they're equal) is absent
	//    from it. A document's FilePath is its relpath, so this is also
	//    the natural index for a path lookup.
	for _, cand := range []string{doc, doc + ".md", doc + ".markdown"} {
		for _, n := range view.nodesByPath[cand] {
			if isDoc(n) {
				return []graphNode{n}
			}
		}
	}

	// 2) Unique basename match across document nodes.
	base := strings.ToLower(doc)
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	if dot := strings.LastIndex(base, "."); dot >= 0 {
		base = base[:dot]
	}
	var matches []graphNode
	for _, n := range view.docNodes() {
		nb := strings.ToLower(n.Name)
		if dot := strings.LastIndex(nb, "."); dot >= 0 {
			nb = nb[:dot]
		}
		if nb == base {
			matches = append(matches, n)
		}
	}
	if len(matches) == 1 {
		return matches
	}
	// Ambiguous (or zero) basename → caller reports not-found; exact
	// relpath is the disambiguator.
	return nil
}

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
	st, err := store.OpenWith(ctx, p.DBPath, s.StoreOpts)
	if err != nil {
		return nil, CyclesOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
	}
	defer func() { _ = st.Close() }()

	view, err := loadGraphView(ctx, st)
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
	st, err := store.OpenWith(ctx, p.DBPath, s.StoreOpts)
	if err != nil {
		return nil, PathOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
	}
	defer func() { _ = st.Close() }()

	view, err := loadGraphView(ctx, st)
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

// ─── tool: graph_diff ─────────────────────────────────────────────────────

type DiffInput struct {
	Ref         string `json:"ref,omitempty" jsonschema:"git ref to diff against (default 'HEAD~1'); supports any ref git understands"`
	MaxDepth    int    `json:"max_depth,omitempty" jsonschema:"BFS depth for blast-radius traversal (default 2, max 5)"`
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
}

type DiffOutput struct {
	Status       string       `json:"status"` // "ok" | "no-index" | "no-graph" | "no-changes" | "error"
	Hint         string       `json:"hint,omitempty"`
	Project      string       `json:"project,omitempty"`
	Ref          string       `json:"ref,omitempty"`
	ChangedFiles []string     `json:"changed_files,omitempty"`
	MaxDepth     int          `json:"max_depth,omitempty"`
	Total        int          `json:"total"`
	Truncated    bool         `json:"truncated,omitempty"`
	Nodes        []ImpactNode `json:"nodes,omitempty"`
}

func (s *Server) GraphDiff(ctx context.Context, in DiffInput) (DiffOutput, error) {
	_, out, err := s.graphDiff(ctx, nil, in)
	return out, err
}

func (s *Server) graphDiff(ctx context.Context, _ *sdk.CallToolRequest, in DiffInput) (*sdk.CallToolResult, DiffOutput, error) {
	p, hint := s.resolveProject(in.ProjectRoot)
	if hint != "" {
		return nil, DiffOutput{Status: "error", Hint: hint}, nil
	}
	if _, err := os.Stat(p.DBPath); errors.Is(err, os.ErrNotExist) {
		return nil, DiffOutput{Status: "no-index", Project: p.Root,
			Hint: fmt.Sprintf("no index for %s — run `dex index %s` first.", p.Root, p.Root)}, nil
	}

	ref := strings.TrimSpace(in.Ref)
	if ref == "" {
		ref = "HEAD~1"
	}
	maxDepth := in.MaxDepth
	if maxDepth <= 0 {
		maxDepth = 2
	}
	if maxDepth > 5 {
		maxDepth = 5
	}

	// Run git diff to collect changed files relative to the project root.
	changedFiles, err := gitDiffFiles(p.Root, ref)
	if err != nil {
		return nil, DiffOutput{Status: "error", Project: p.Root,
			Hint: fmt.Sprintf("git diff --name-only %s: %v", ref, err)}, nil
	}
	if len(changedFiles) == 0 {
		return nil, DiffOutput{Status: "no-changes", Project: p.Root, Ref: ref,
			Hint: fmt.Sprintf("no files changed between %s and HEAD", ref)}, nil
	}

	st, err := store.OpenWith(ctx, p.DBPath, s.StoreOpts)
	if err != nil {
		return nil, DiffOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
	}
	defer func() { _ = st.Close() }()

	view, err := loadGraphView(ctx, st)
	if err != nil {
		return nil, DiffOutput{Status: "error", Hint: fmt.Sprintf("load graph: %v", err)}, nil
	}
	if view == nil {
		return nil, DiffOutput{Status: "no-graph", Project: p.Root,
			Hint: fmt.Sprintf("graph not indexed for %s — run `dex index %s --graph=only`.", p.Root, p.Root)}, nil
	}

	// Collect all graph nodes whose file path matches one of the changed files.
	changedSet := make(map[string]bool, len(changedFiles))
	for _, f := range changedFiles {
		changedSet[f] = true
		// Also try relative path without leading ./
		changedSet[strings.TrimPrefix(f, "./")] = true
	}
	var seeds []graphNode
	seen := map[string]bool{}
	for _, n := range view.nodesByID {
		rel := n.FilePath
		if !changedSet[rel] {
			continue
		}
		if seen[n.ID] {
			continue
		}
		// Only seed on function/method symbols — not imports or types.
		if n.Kind != graph.NodeFunction && n.Kind != graph.NodeMethod {
			continue
		}
		seen[n.ID] = true
		seeds = append(seeds, n)
	}

	const maxBlastNodes = 300
	nodes := computeImpactNodes(view, seeds, maxDepth)

	out := DiffOutput{
		Status: "ok", Project: p.Root, Ref: ref,
		ChangedFiles: changedFiles,
		MaxDepth:     maxDepth,
		Total:        len(nodes),
	}
	if len(nodes) > maxBlastNodes {
		nodes = nodes[:maxBlastNodes]
		out.Truncated = true
	}
	out.Nodes = nodes
	return nil, out, nil
}

// gitDiffFiles runs `git diff --name-only <ref> HEAD` in root and returns
// the list of changed file paths relative to root.
func gitDiffFiles(root, ref string) ([]string, error) {
	mkCmd := func(args ...string) *exec.Cmd {
		c := exec.Command("git", args...) // #nosec G204
		c.Dir = root
		return c
	}
	out, err := mkCmd("diff", "--name-only", ref, "HEAD").Output()
	if err != nil {
		// Try without HEAD in case HEAD == ref (initial commit, detached HEAD)
		if out2, err2 := mkCmd("diff", "--name-only", ref).Output(); err2 == nil {
			out = out2
		} else {
			return nil, err
		}
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}

// ─── tool: graph_communities ──────────────────────────────────────────────

type CommunitiesInput struct {
	MinMembers  int    `json:"min_members,omitempty" jsonschema:"min community size to include (default 3)"`
	K           int    `json:"k,omitempty" jsonschema:"max communities to return (default 20, max 50)"`
	TopK        int    `json:"top_k,omitempty" jsonschema:"max members to include per community (default 10)"`
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
}

type CommunityMember struct {
	QualifiedName   string  `json:"qualified_name"`
	Package         string  `json:"package,omitempty"`
	Kind            string  `json:"kind"`
	Path            string  `json:"path"`
	StartLine       int     `json:"start_line"`
	InDegree        int     `json:"in_degree,omitempty"`
	CrossPkgCallers int     `json:"cross_pkg_callers,omitempty"`
	PageRank        float64 `json:"page_rank,omitempty"`
}

type Community struct {
	ID      int               `json:"id"`
	Size    int               `json:"size"`
	Members []CommunityMember `json:"members"`
}

type CommunitiesOutput struct {
	Status      string      `json:"status"` // "ok" | "no-index" | "no-graph" | "error"
	Hint        string      `json:"hint,omitempty"`
	Project     string      `json:"project,omitempty"`
	Total       int         `json:"total"`
	Truncated   bool        `json:"truncated,omitempty"`
	Communities []Community `json:"communities,omitempty"`
}

func (s *Server) GraphCommunities(ctx context.Context, in CommunitiesInput) (CommunitiesOutput, error) {
	_, out, err := s.graphCommunities(ctx, nil, in)
	return out, err
}

func (s *Server) graphCommunities(ctx context.Context, _ *sdk.CallToolRequest, in CommunitiesInput) (*sdk.CallToolResult, CommunitiesOutput, error) {
	p, hint := s.resolveProject(in.ProjectRoot)
	if hint != "" {
		return nil, CommunitiesOutput{Status: "error", Hint: hint}, nil
	}
	if _, err := os.Stat(p.DBPath); errors.Is(err, os.ErrNotExist) {
		return nil, CommunitiesOutput{Status: "no-index", Project: p.Root,
			Hint: fmt.Sprintf("no index for %s — run `dex index %s` first.", p.Root, p.Root)}, nil
	}

	minMembers := in.MinMembers
	if minMembers <= 0 {
		minMembers = 3
	}
	k := in.K
	if k <= 0 {
		k = 20
	}
	if k > 50 {
		k = 50
	}
	topK := in.TopK
	if topK <= 0 {
		topK = 10
	}
	if topK > 50 {
		topK = 50
	}

	st, err := store.OpenWith(ctx, p.DBPath, s.StoreOpts)
	if err != nil {
		return nil, CommunitiesOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
	}
	defer func() { _ = st.Close() }()

	communities, err := st.GraphCommunities(ctx, minMembers, k*2) // fetch 2× to allow topK trim
	if err != nil {
		return nil, CommunitiesOutput{Status: "error", Hint: fmt.Sprintf("communities: %v", err)}, nil
	}

	if len(communities) == 0 {
		return nil, CommunitiesOutput{
			Status: "no-graph", Project: p.Root,
			Hint: "no community data — run `dex index . --graph=only` to build the call graph and compute communities.",
		}, nil
	}

	out := CommunitiesOutput{Status: "ok", Project: p.Root, Total: len(communities)}
	if len(communities) > k {
		communities = communities[:k]
		out.Truncated = true
	}
	for _, c := range communities {
		mc := Community{ID: c.ID, Size: len(c.Members)}
		members := c.Members
		if len(members) > topK {
			members = members[:topK]
		}
		for _, m := range members {
			mc.Members = append(mc.Members, CommunityMember{
				QualifiedName:   m.QualifiedName,
				Package:         m.PackagePath,
				Kind:            m.Kind,
				Path:            m.FilePath,
				StartLine:       m.StartLine,
				InDegree:        m.InDegree,
				CrossPkgCallers: m.CrossPkgCallers,
				PageRank:        m.PageRank,
			})
		}
		out.Communities = append(out.Communities, mc)
	}
	return nil, out, nil
}
