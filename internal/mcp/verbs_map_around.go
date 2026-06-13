package mcp

import (
	"context"
	"fmt"

	"github.com/alehatsman/dex/internal/codemap"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// aroundNeighborK is the default number of call-graph neighbors pulled per
// direction (callers, callees) when MapInput.K is unset. Generous so the
// region is reasonably complete; the render budget still bounds the output.
const aroundNeighborK = 25

// mapAround renders a task-focused region instead of the global L0/L1 overview
// (issue #347, story 5). mapVerb has already validated that exactly one of
// Around / AroundDiff is set and that neither is combined with Cluster.
func mapAround(ctx context.Context, h toolSurface, req *sdk.CallToolRequest, in MapInput) (*sdk.CallToolResult, MapOutput, error) {
	if in.AroundDiff != "" {
		return mapAroundDiff(ctx, h, req, in)
	}
	return mapAroundQuery(ctx, h, req, in)
}

// mapAroundQuery renders the seed symbol's direct call-graph neighborhood —
// callers ∪ callees — as one region. This is the breadth use-case (#351 phase
// 2): a single map call enumerates a hub's neighbors instead of repeated
// find/trace. Seed resolution reuses the same name matching graphCallers /
// graphCallees apply (bare, receiver-qualified, or package-tail-qualified).
func mapAroundQuery(ctx context.Context, h toolSurface, req *sdk.CallToolRequest, in MapInput) (*sdk.CallToolResult, MapOutput, error) {
	k := in.K
	if k == 0 {
		k = aroundNeighborK
	}
	cin := CallEdgeInput{Name: in.Around, ProjectRoot: in.ProjectRoot, K: k}

	_, callers, err := h.graphCallers(ctx, req, cin)
	if err != nil {
		return nil, MapOutput{Status: "error", Hint: err.Error()}, err
	}
	_, callees, err := h.graphCallees(ctx, req, cin)
	if err != nil {
		return nil, MapOutput{Status: "error", Hint: err.Error()}, err
	}

	// Surface a hard backend failure (no-index / no-graph / error) rather than
	// masking it as an empty region. Only when BOTH lanes report not-found does
	// the seed itself not exist.
	if st := firstHardStatus(callers.Status, callees.Status); st != "" {
		return nil, MapOutput{Status: st, Hint: firstNonEmpty(callers.Hint, callees.Hint)}, nil
	}
	if callers.Status == "not-found" && callees.Status == "not-found" {
		return nil, MapOutput{
			Status: "not-found",
			Hint:   fmt.Sprintf("symbol %q not found in the call graph", in.Around),
		}, nil
	}

	return nil, MapOutput{
		Status: "ok",
		Zoom:   "around",
		Map:    codemap.RenderAround(AroundTitle(in.Around), AroundSymbols(callers, callees), in.Budget),
	}, nil
}

// mapAroundDiff renders the blast radius of a git diff as one region (issue
// #347, story 5). The radius comes straight from graphDiff; AroundDiff carries
// the ref to diff against.
func mapAroundDiff(ctx context.Context, h toolSurface, req *sdk.CallToolRequest, in MapInput) (*sdk.CallToolResult, MapOutput, error) {
	_, d, err := h.graphDiff(ctx, req, DiffInput{Ref: in.AroundDiff, ProjectRoot: in.ProjectRoot})
	if err != nil {
		return nil, MapOutput{Status: "error", Hint: err.Error()}, err
	}
	if d.Status != "ok" {
		return nil, MapOutput{Status: d.Status, Hint: d.Hint}, nil
	}

	ref := d.Ref
	if ref == "" {
		ref = in.AroundDiff
	}
	return nil, MapOutput{
		Status: "ok",
		Zoom:   "around",
		Map:    codemap.RenderAround(DiffTitle(ref), DiffSymbols(d), in.Budget),
	}, nil
}

// AroundSymbols assembles the deduped region for an --around query: the seed
// targets plus their callers and callees, deduped on qualified name + path.
// Shared by the map verb and the `dex map --around` CLI (issue #347, story 5).
func AroundSymbols(callers, callees CallEdgeOutput) []codemap.Symbol {
	seen := make(map[string]bool)
	var syms []codemap.Symbol
	add := func(s codemap.Symbol) {
		if s.QualifiedName == "" {
			return
		}
		key := s.QualifiedName + "\x00" + s.Path
		if seen[key] {
			return
		}
		seen[key] = true
		syms = append(syms, s)
	}
	for _, t := range append(callers.Targets, callees.Targets...) {
		add(codemap.Symbol{QualifiedName: t.QualifiedName, Kind: t.Kind, Pkg: t.Package, Path: t.Path, Line: t.StartLine})
	}
	for _, c := range append(callers.Hits, callees.Hits...) {
		add(codemap.Symbol{QualifiedName: c.QualifiedName, Kind: c.Kind, Pkg: c.Package, Path: c.Path, Line: c.StartLine})
	}
	return syms
}

// DiffSymbols adapts a diff's blast-radius nodes to region symbols (issue #347).
func DiffSymbols(d DiffOutput) []codemap.Symbol {
	syms := make([]codemap.Symbol, 0, len(d.Nodes))
	for _, n := range d.Nodes {
		syms = append(syms, codemap.Symbol{
			QualifiedName: n.QualifiedName,
			Kind:          n.Kind,
			Pkg:           n.Package,
			Path:          n.Path,
			Line:          n.StartLine,
			PageRank:      n.PageRank,
		})
	}
	return syms
}

// AroundTitle is the region header for an --around query.
func AroundTitle(seed string) string { return fmt.Sprintf("around %s — callers ∪ callees", seed) }

// DiffTitle is the region header for an --around-diff blast radius.
func DiffTitle(ref string) string { return fmt.Sprintf("around diff %s — blast radius", ref) }

// firstHardStatus returns the first status that is a hard failure — anything
// other than "ok", "not-found", or empty — so an around query reports a missing
// index or absent graph instead of silently rendering nothing.
func firstHardStatus(statuses ...string) string {
	for _, s := range statuses {
		if s != "" && s != "ok" && s != "not-found" {
			return s
		}
	}
	return ""
}

// firstNonEmpty returns the first non-empty string, for picking a hint across
// the two call-graph lanes.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
