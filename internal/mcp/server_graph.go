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
	for _, n := range view.nodesByID {
		if isGoPackageNode(n) && n.PackagePath != "" {
			internal[n.PackagePath] = struct{}{}
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
				Role:          formatRole(peer.Name, peer.InDegree, peer.OutDegree, peer.CrossPkgCallers),
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

// ─── tools: graph_links / graph_backlinks ─────────────────────────────────

type DocLinkInput struct {
	Doc         string `json:"doc" jsonschema:"path to a markdown document relative to the project root (e.g. 'docs/spec.md'); a bare basename like 'spec' is also accepted when unambiguous"`
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
	K           int    `json:"k,omitempty" jsonschema:"max hits to return (default 50, max 200)"`
}

// DocLink is one endpoint of a doc-graph edge: the document on the other
// end, plus the file:line of the link expression that produced the edge.
type DocLink struct {
	Doc          string `json:"doc"`  // qualified name (relpath) of the peer document
	Name         string `json:"name"` // basename of the peer document
	Kind         string `json:"kind"` // "links" | "wikilinks"
	LinkSitePath string `json:"link_site_path,omitempty"`
	LinkSiteLine int    `json:"link_site_line,omitempty"`
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
// returns the peer documents, keeping only `links`/`wikilinks` edges so
// code edges (`calls`/`imports`/…) never leak in. backlinks=false walks
// outgoing edges (docs the target points to); backlinks=true walks
// incoming edges (docs that point to the target). Hits are deduped on
// (peer doc, edge kind, link-site line), sorted deterministically, and
// capped at k. Pure over view — unit-testable off a hand-built graph.
func collectDocEdges(view *graphView, targets []graphNode, backlinks bool, k int) []DocLink {
	seen := map[string]bool{}
	var hits []DocLink
	for _, t := range targets {
		edges := view.edgesBySrc[t.ID]
		if backlinks {
			edges = view.edgesByDst[t.ID]
		}
		for _, e := range edges {
			if e.Kind != graph.EdgeLinks && e.Kind != graph.EdgeWikilinks {
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
			key := peer.ID + "|" + string(e.Kind) + "|" + e.FilePath + ":" + fmt.Sprint(e.StartLine)
			if seen[key] {
				continue
			}
			seen[key] = true
			hits = append(hits, DocLink{
				Doc:          peer.QualifiedName,
				Name:         peer.Name,
				Kind:         string(e.Kind),
				LinkSitePath: e.FilePath,
				LinkSiteLine: e.StartLine,
			})
		}
	}
	// Deterministic order: by peer doc path, then kind, then link-site line.
	sort.SliceStable(hits, func(i, j int) bool {
		a, b := hits[i], hits[j]
		if a.Doc != b.Doc {
			return a.Doc < b.Doc
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.LinkSiteLine < b.LinkSiteLine
	})
	if len(hits) > k {
		hits = hits[:k]
	}
	return hits
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

	// 1) Exact relpath (qualified name) match, with/without an extension.
	for _, cand := range []string{doc, doc + ".md", doc + ".markdown"} {
		for _, n := range view.nodesByQualified[cand] {
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
