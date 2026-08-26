package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/alehatsman/dex/internal/graph"
	"github.com/alehatsman/dex/internal/graphquery"
	"github.com/alehatsman/dex/internal/source"
	"github.com/alehatsman/dex/internal/store"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func (s *Server) GraphCallers(ctx context.Context, in CallEdgeInput) (CallEdgeOutput, error) {
	_, out, err := s.graphCallers(ctx, nil, in)
	return out, err
}

func (s *Server) GraphCallees(ctx context.Context, in CallEdgeInput) (CallEdgeOutput, error) {
	_, out, err := s.graphCallees(ctx, nil, in)
	return out, err
}

// ─── tools: graph_callers / graph_callees ─────────────────────────────────

type CallEdgeInput struct {
	Name        string `json:"name" jsonschema:"symbol to query: bare ('Foo'), receiver-qualified ('(*Server).RunStdio'), or package-tail-qualified ('mcp.NewServer')"`
	Package     string `json:"package,omitempty" jsonschema:"optional package path filter when the same name is defined in multiple packages"`
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project or git worktree you are working in. The server cannot see your shell's directory; when working in a worktree different from where the server started, pass that worktree's path"`
	K           int    `json:"k,omitempty" jsonschema:"max hits to return (default 12, max 50)"`
	Verbose     bool   `json:"verbose,omitempty" jsonschema:"return the full enclosing function body per hit instead of a window centred on the call site (default false)"`
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
	Role string `json:"role,omitempty"`
	// Via is set on an interface-DISPATCH caller (#604): the qualified name of
	// the interface method this call reaches the target through (e.g.
	// "(toolSurface).Run"). The static `calls` edge lands on the interface
	// method, not the concrete one, so without this expansion the caller is
	// invisible. Empty for a direct static caller.
	Via string `json:"via,omitempty"`
	// Content is, by default, a small window centred on the call site (#486)
	// — the call expression is the answer, not the whole enclosing function.
	// IMPORTANT (#231): the window always lives at CallSitePath:CallSiteLine —
	// on a callers query that's the same file as the peer's own definition
	// (Path/StartLine/EndLine), but on a CALLEES query the call expression
	// lives in the QUERIED symbol's body, not the callee's, so Content shows
	// "how the queried symbol invokes this callee", not the callee's own
	// source. Path/StartLine/EndLine are always the callee's own definition —
	// query() that location directly to read the callee's body. Set Verbose
	// to get the full enclosing body instead of the windowed call site.
	Content string `json:"content,omitempty"`
	// ContentStartLine is the first source line of Content (the window's top,
	// or the enclosing symbol's start line in verbose mode) — a position in
	// CallSitePath, NOT necessarily in Path (see Content's callees note above).
	ContentStartLine int  `json:"content_start_line,omitempty"`
	Truncated        bool `json:"truncated,omitempty"`
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
	Hits    []CallSite    `json:"hits"`
	// Risk is set on callers-direction queries only: Low | Medium | High |
	// Critical, derived from a BFS at depth 5 over the callers direction.
	Risk string `json:"risk,omitempty"`
	// UnresolvedInbound lists known import edges into the target's package that
	// the resolver could not bind to a symbol (build-mediated / workspace
	// subpath). Populated by the CLI callers path for parity with the MCP trace
	// verb; omitted (nil) on the MCP graph_callers tool. #130.
	UnresolvedInbound []store.UnresolvedInbound `json:"unresolved_inbound,omitempty"`
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
	p, hint := s.resolveProject(ctx, in.ProjectRoot)
	if hint != "" {
		return nil, CallEdgeOutput{Status: "error", Hint: hint}, nil
	}
	if _, err := os.Stat(p.DBPath); errors.Is(err, os.ErrNotExist) {
		return nil, CallEdgeOutput{Status: "no-index", Project: p.Root,
			Hint: fmt.Sprintf("no index for %s — run `dex index %s` first.", p.Root, p.Root)}, nil
	}
	st, err := s.openStore(p.DBPath)
	if err != nil {
		return nil, CallEdgeOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
	}

	view, err := s.cachedLoadGraphView(ctx, st, p.DBPath)
	if err != nil {
		return nil, CallEdgeOutput{Status: "error", Hint: fmt.Sprintf("load graph: %v", err)}, nil
	}
	if view == nil {
		return nil, CallEdgeOutput{Status: "no-graph", Project: p.Root,
			Hint: fmt.Sprintf("graph not indexed for %s — run `dex index %s --graph=only`.", p.Root, p.Root)}, nil
	}
	if len(view.EdgesByKind[graph.EdgeCalls]) == 0 {
		return nil, CallEdgeOutput{Status: "no-graph", Project: p.Root,
			Hint: "graph has no `calls` edges — reindex the project with this release (`dex index . --graph=only`) to extract them."}, nil
	}

	targets := graphquery.ResolveCallTargets(view, in.Name, in.Package)
	if len(targets) == 0 {
		return nil, CallEdgeOutput{Status: "not-found", Project: p.Root,
			Hint: notFoundHint(view, in.Name, in.Package)}, nil
	}

	k := in.K
	if k <= 0 {
		k = 12
	}
	if k > 50 {
		k = 50
	}

	out := CallEdgeOutput{Status: "ok", Project: p.Root, Hits: []CallSite{}}
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
	addHit := func(peer graphquery.Node, e graphquery.Edge, via string) {
		// Dedup on (peer node id, call-site file+line). Different call sites from
		// the same caller are distinct hits.
		key := peer.ID + "@" + e.FilePath + ":" + fmt.Sprint(e.StartLine)
		if seen[key] {
			return
		}
		seen[key] = true
		out.Hits = append(out.Hits, CallSite{
			QualifiedName: peer.QualifiedName,
			Package:       peer.PackagePath,
			Kind:          string(peer.Kind),
			Path:          peer.FilePath,
			StartLine:     peer.StartLine,
			EndLine:       peer.EndLine,
			CallSitePath:  e.FilePath,
			CallSiteLine:  e.StartLine,
			Role:          formatRole(peer.Name, peer.InDegree, peer.OutDegree, peer.CrossPkgCallers, peer.Betweenness),
			Via:           via,
		})
	}
	for _, t := range targets {
		var edges []graphquery.Edge
		if callers {
			edges = view.EdgesByDst[t.ID]
		} else {
			edges = view.EdgesBySrc[t.ID]
		}
		for _, e := range edges {
			if e.Kind != graph.EdgeCalls {
				continue
			}
			peerID := e.SrcID
			if !callers {
				peerID = e.DstID
			}
			if peer, ok := view.NodesByID[peerID]; ok {
				addHit(peer, e, "")
			}
		}
	}

	// Interface-dispatch callers (#604): a call through an interface value lands
	// on the interface method node in the graph, not the concrete method, so the
	// static loop above misses it. Surfaced (tagged with `via`) for the callers
	// direction only — a concrete method's callees are already static.
	if callers {
		for _, dc := range interfaceDispatchCallers(view, targets) {
			addHit(dc.peer, dc.edge, dc.via)
		}
	}

	// Sort hits by peer centrality, then by path/line for determinism.
	// peerCentrality is a closure over view.NodesByID so we don't
	// re-resolve per hit. PageRank dominates; in_degree breaks ties
	// for peers that didn't pick up rank (e.g. callees with no
	// incoming edges in the indexed slice).
	peerCentrality := func(h CallSite) (float64, int) {
		// Resolve peer node by qualified name + package — the same key
		// we used when populating the hit.
		for _, n := range view.NodesByQualified[h.QualifiedName] {
			if n.PackagePath == h.Package {
				return n.PageRank, n.InDegree
			}
		}
		for _, n := range view.NodesByName[h.QualifiedName] {
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

	if len(out.Hits) == 0 {
		out.Hint = emptyCallHint(view, targets, callers)
	}

	// Risk classification is only meaningful for the callers direction — it
	// answers "how much of the codebase transitively depends on this symbol".
	if callers {
		riskCount := graphquery.TransitiveCallerCount(view, targets, 5)
		out.Risk = graphquery.RiskLevel(riskCount)
	}

	inlineCallSites(out.Hits, p.Root, in.Verbose)

	return nil, out, nil
}

// notFoundHint builds the not-found hint shared by trace/impact/path. When a
// package filter was supplied and the bare name DOES resolve in other packages,
// it names those packages — the filter was too narrow, not the symbol missing
// (#583). Without that signal it falls back to the generic spelling hint.
func notFoundHint(view *graphquery.View, name, pkgFilter string) string {
	if strings.TrimSpace(pkgFilter) != "" {
		if cands := graphquery.PkgFilterCandidates(view, name); len(cands) > 0 {
			return fmt.Sprintf("name=%q exists but no definition is in package=%q — it lives in %s; "+
				"pass one of those (a path suffix like the tail segment works) or drop the package filter",
				name, pkgFilter, strings.Join(cands, ", "))
		}
	}
	return fmt.Sprintf("no graph node matches name=%q — try the bare identifier or the "+
		"receiver-qualified form like '(*Type).Method'", name)
}

// emptyCallHint explains a zero-hit calls result. On tree-sitter (name-based)
// languages recall is incomplete, so an empty result is not proof of none; on
// Go a zero-callers result on an exported symbol still warns about interface/
// reflection dispatch (#485). Returns "" when there's nothing useful to say.
func emptyCallHint(view *graphquery.View, targets []graphquery.Node, callers bool) string {
	rel := "callees"
	if callers {
		rel = "callers"
	}
	if h := nameBasedEmptyHint(targets, rel); h != "" {
		return h
	}
	if callers {
		return zeroCallerHint(view, targets)
	}
	return ""
}

// inlineCallSites fills each hit's Content so the agent doesn't need a
// follow-up Read. #486: by default it centres a small window on the call site
// — for a who-calls-X query the call expression is the answer; the full
// enclosing body (which can be 150+ lines) is noise. Verbose restores the
// whole-function slice for the rare case it's wanted.
func inlineCallSites(hits []CallSite, root string, verbose bool) {
	const (
		maxHitLines    = 30
		maxHitBytes    = 2 * 1024
		callSiteWindow = 6 // lines of context on each side of the call site
	)
	for i := range hits {
		abs := hits[i].Path
		// In windowed mode the call expression lives in the caller's file
		// (CallSitePath), which can differ from the hit's own Path.
		if !verbose && hits[i].CallSiteLine > 0 && hits[i].CallSitePath != "" {
			abs = hits[i].CallSitePath
		}
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(root, abs)
		}

		startLine, endLine := callSiteRange(hits[i], verbose, callSiteWindow)
		content, truncated, err := source.ReadLineRange(abs, startLine, endLine, maxHitLines, maxHitBytes)
		if err == nil {
			hits[i].Content = content
			hits[i].ContentStartLine = startLine
			hits[i].Truncated = truncated
		}
	}
}

// callSiteRange picks the source line range to inline for a hit (#486).
// Default: a [line-window, line+window] slice centred on the call site, so a
// who-calls-X hit returns the call expression plus a little context instead of
// the whole enclosing function. Verbose (or a missing call-site line) falls
// back to the enclosing symbol's full [StartLine, EndLine] body.
func callSiteRange(hit CallSite, verbose bool, window int) (start, end int) {
	if verbose || hit.CallSiteLine <= 0 {
		return hit.StartLine, hit.EndLine
	}
	start = hit.CallSiteLine - window
	if start < 1 {
		start = 1
	}
	return start, hit.CallSiteLine + window
}
