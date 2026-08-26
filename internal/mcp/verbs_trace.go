package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/alehatsman/dex/internal/retrieve"
	"github.com/alehatsman/dex/internal/store"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TraceInput drives the trace verb — a single entry point for call-graph
// navigation from a symbol. direction selects the traversal; the call-edge
// directions (callers/callees) and path share one symbol-name input.
type TraceInput struct {
	Symbol      string `json:"symbol" jsonschema:"symbol to trace: bare ('Foo'), receiver-qualified ('(*Server).Run'), or package-tail-qualified ('mcp.NewServer')"`
	Direction   string `json:"direction,omitempty" jsonschema:"'callers' (default — who calls it), 'callees' (what it calls), 'path' (shortest call route to the 'to' symbol), or 'impact' (transitive caller blast-radius with risk tier + tests_to_run)"`
	To          string `json:"to,omitempty" jsonschema:"destination symbol; required when direction=path"`
	Package     string `json:"package,omitempty" jsonschema:"optional package-path filter when the same name is defined in multiple packages"`
	MaxDepth    int    `json:"max_depth,omitempty" jsonschema:"BFS depth limit: path (default 8, max 15); impact (default 3, max 5). Ignored for callers/callees"`
	K           int    `json:"k,omitempty" jsonschema:"max hits to return: callers/callees (default 12, max 50); impact nodes per depth (default 8, max 200)"`
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project or git worktree you are working in. The server cannot see your shell's directory; when working in a worktree different from where the server started, pass that worktree's path"`
}

// TraceOutput is the unified envelope across the four directions. The
// call-edge directions fill Targets+Hits; path fills Src/Dst/Path; impact fills
// Nodes+MaxDepth+Total+Elided+TestsToRun. Empty fields are omitted, so each
// direction's response stays compact.
type TraceOutput struct {
	Direction string        `json:"direction"`
	Status    string        `json:"status"` // "ok" | "no-index" | "no-graph" | "not-found" | "no-path" | "error"
	Hint      string        `json:"hint,omitempty"`
	Project   string        `json:"project,omitempty"`
	Targets   []TargetMatch `json:"targets,omitempty"` // callers/callees/impact: resolved interpretations of `symbol`
	Hits      []CallSite    `json:"hits"`              // callers/callees: the call-edge endpoints
	Src       string        `json:"src,omitempty"`     // path
	Dst       string        `json:"dst,omitempty"`     // path
	Path      []PathHop     `json:"path,omitempty"`    // path: ordered hops
	// Risk is set for direction=callers and direction=impact: Low | Medium |
	// High | Critical.
	Risk string `json:"risk,omitempty"`
	// impact: the transitive caller blast-radius.
	Nodes      []ImpactNode   `json:"nodes,omitempty"`
	MaxDepth   int            `json:"max_depth,omitempty"`
	Total      int            `json:"total,omitempty"`
	Truncated  bool           `json:"truncated,omitempty"`
	Elided     []DepthElision `json:"elided,omitempty"`
	TestsToRun []string       `json:"tests_to_run,omitempty"`
	// Recall is "partial" when the graph result may undercount call sites —
	// non-Go (tree-sitter) extractors have incomplete recall, so a non-empty
	// result is still not a complete blast radius. Check grep_hits for
	// additional candidate sites surfaced by a name-based grep sweep.
	Recall   string      `json:"recall,omitempty"`
	GrepHits []GrepMatch `json:"grep_hits,omitempty"`
	// UnresolvedInbound lists known import edges into the target symbol's package
	// that the resolver could not bind to a symbol (build-mediated / workspace
	// subpath, e.g. `@acme/common/Uuid`). Their specifier and the target's name
	// differ, so the grep sweep above cannot see them — surfacing them counted
	// turns a silent undercount into an actionable one (#130). Grep the specifier.
	UnresolvedInbound []store.UnresolvedInbound `json:"unresolved_inbound,omitempty"`
}

// traceHandler adapts traceVerb to the SDK handler shape, capturing h.
func traceHandler(h toolSurface) func(context.Context, *sdk.CallToolRequest, TraceInput) (*sdk.CallToolResult, TraceOutput, error) {
	return func(ctx context.Context, req *sdk.CallToolRequest, in TraceInput) (*sdk.CallToolResult, TraceOutput, error) {
		return traceVerb(ctx, h, req, in)
	}
}

// Trace runs the trace verb for callers without an SDK request — the REST
// `/trace` route. It composes over the local *Server exactly like the stdio
// `trace` tool, so both transports agree.
func (s *Server) Trace(ctx context.Context, in TraceInput) (TraceOutput, error) {
	_, out, err := traceVerb(ctx, s, nil, in)
	return out, err
}

// traceVerb dispatches a trace call to the underlying graph handler for the
// requested direction and folds its result into TraceOutput.
func traceVerb(ctx context.Context, h toolSurface, req *sdk.CallToolRequest, in TraceInput) (*sdk.CallToolResult, TraceOutput, error) {
	dir := strings.ToLower(strings.TrimSpace(in.Direction))
	if dir == "" {
		dir = "callers"
	}
	switch dir {
	case "callers", "callees":
		ce := CallEdgeInput{Name: in.Symbol, Package: in.Package, ProjectRoot: in.ProjectRoot, K: in.K}
		var out CallEdgeOutput
		var err error
		if dir == "callers" {
			_, out, err = h.graphCallers(ctx, req, ce)
		} else {
			_, out, err = h.graphCallees(ctx, req, ce)
		}
		tOut := TraceOutput{
			Direction: dir,
			Status:    out.Status,
			Hint:      out.Hint,
			Project:   out.Project,
			Targets:   out.Targets,
			Hits:      out.Hits,
			Risk:      out.Risk,
		}
		if out.Status == "ok" && hasNonGoTarget(out.Targets) {
			if len(out.Hits) > 0 {
				augmentPartialRecall(ctx, h, in.Symbol, in.ProjectRoot, &tOut)
			}
			// Only for callers: unresolved *inbound* imports are potential hidden
			// callers, and matter most when resolved callers are few or zero
			// (that's exactly the undercount). They're irrelevant to callees.
			if dir == "callers" {
				foldUnresolvedInbound(ctx, h, in.ProjectRoot, &tOut)
			}
		}
		return nil, tOut, err
	case "path":
		if strings.TrimSpace(in.To) == "" {
			return nil, TraceOutput{Direction: dir, Status: "error", Hint: "direction=path requires `to` (destination symbol)"}, nil
		}
		pi := PathInput{Src: in.Symbol, Dst: in.To, Package: in.Package, MaxDepth: in.MaxDepth, ProjectRoot: in.ProjectRoot}
		_, out, err := h.graphPath(ctx, req, pi)
		return nil, TraceOutput{
			Direction: dir,
			Status:    out.Status,
			Hint:      out.Hint,
			Project:   out.Project,
			Src:       out.Src,
			Dst:       out.Dst,
			Path:      out.Path,
		}, err
	case "impact":
		ii := ImpactInput{Name: in.Symbol, Package: in.Package, MaxDepth: in.MaxDepth, K: in.K, ProjectRoot: in.ProjectRoot}
		_, out, err := h.graphImpact(ctx, req, ii)
		tOut := TraceOutput{
			Direction:  dir,
			Status:     out.Status,
			Hint:       out.Hint,
			Project:    out.Project,
			Targets:    out.Targets,
			Risk:       out.Risk,
			Nodes:      out.Nodes,
			MaxDepth:   out.MaxDepth,
			Total:      out.Total,
			Truncated:  out.Truncated,
			Elided:     out.Elided,
			TestsToRun: out.TestsToRun,
		}
		if out.Status == "ok" && hasNonGoTarget(out.Targets) {
			if out.Total > 0 {
				markImpactPartialRecall(&tOut)
			}
			foldUnresolvedInbound(ctx, h, in.ProjectRoot, &tOut)
		}
		return nil, tOut, err
	default:
		return nil, TraceOutput{Direction: dir, Status: "error", Hint: "direction must be one of: callers, callees, path, impact"}, nil
	}
}

// hasNonGoTarget reports whether any resolved target lives in a non-Go file.
// Non-Go edges are name-based (tree-sitter), so recall is incomplete.
func hasNonGoTarget(targets []TargetMatch) bool {
	for _, t := range targets {
		if t.Path != "" && !strings.HasSuffix(t.Path, ".go") {
			return true
		}
	}
	return false
}

// markImpactPartialRecall tags a non-empty impact blast radius on a non-Go
// (tree-sitter) target as partial-recall. Unlike callers/callees, no grep
// sweep is run: impact is a *transitive* radius and a single bare-symbol grep
// can't reconstruct it, so approximating would over-claim. The honest signal
// is the flag plus a hint pointing at grep for the edges that matter.
func markImpactPartialRecall(out *TraceOutput) {
	out.Recall = "partial"
	partial := fmt.Sprintf("impact: %d node(s) via name-based (tree-sitter) edges, recall partial — the true blast radius may be larger; verify critical edges with grep on the symbol names", out.Total)
	if out.Hint != "" {
		out.Hint += " | " + partial
	} else {
		out.Hint = partial
	}
}

// augmentPartialRecall runs a grep sweep for the bare symbol name and folds
// the results into out.GrepHits (deduped against existing call-site lines).
// Sets out.Recall = "partial" and annotates out.Hint regardless of grep outcome.
func augmentPartialRecall(ctx context.Context, h toolSurface, symbol, projectRoot string, out *TraceOutput) {
	bare := retrieve.BareSymbolName(symbol)
	if bare == "" {
		out.Recall = "partial"
		return
	}
	pattern := `\b` + bare + `\b`
	_, gout, err := h.searchGrep(ctx, nil, SearchGrepInput{
		ProjectRoot: projectRoot,
		Pattern:     pattern,
		MaxResults:  20,
	})

	out.Recall = "partial"

	var grepN int
	if err == nil && (gout.Status == "ok" || gout.Status == "no-matches") {
		// Dedup: skip grep hits already covered by a graph call-site line.
		seenSite := make(map[string]bool, len(out.Hits))
		for _, h := range out.Hits {
			if h.CallSitePath != "" {
				seenSite[fmt.Sprintf("%s:%d", h.CallSitePath, h.CallSiteLine)] = true
			}
		}
		for _, m := range gout.Matches {
			if !seenSite[fmt.Sprintf("%s:%d", m.Path, m.Line)] {
				out.GrepHits = append(out.GrepHits, m)
			}
		}
		grepN = len(out.GrepHits)
	}

	partial := fmt.Sprintf("graph: %d call-edge(s) (name-based, recall partial); grep: %d more candidate site(s)", len(out.Hits), grepN)
	if out.Hint != "" {
		out.Hint += " | " + partial
	} else {
		out.Hint = partial
	}
}
