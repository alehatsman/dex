// Package mcp provides graph integration for the `ask` router.
//
// internal/graph emits nodes (package/file/function/method/type/
// struct/interface/field/import) and edges (contains/imports/
// has_method/has_field/embeds/implements/calls — the last Go-only).
// The intents and what they get:
//
//	symbol_lookup     — neighbors of the matched symbol (sibling
//	                    methods, fields, embedded types) so the agent
//	                    sees the whole "shape" of a type without
//	                    reading the file.
//	editing_context   — same neighborhood, plus the enclosing type
//	                    so refactors know what else uses the type.
//	architecture      — package/type roll-up for packages surfaced
//	                    by the semantic lane.
//	package_topology  — import edges between packages in the
//	                    semantic neighborhood.
//	callers           — incoming calls edges into matched symbols
//	                    (Go-only; falls back to ripgrep usage list
//	                    for other languages via context.go).
//	callees           — outgoing calls edges from matched symbols.
//
// Loader strategy: a single in-memory view per request. With the
// current scale (~800 nodes for this repo) that's a few hundred KB;
// when it stops fitting we can move to targeted SQL queries.
//
// The enrichment algorithm itself lives in internal/retrieve
// (retrieve.EnrichGraph) over neutral types; this file is the thin
// transport wrapper that converts wire hits to neutral and maps the
// neutral GraphResult back onto the wire ContextOutput.
package mcp

import (
	"github.com/alehatsman/dex/internal/graphquery"
	"github.com/alehatsman/dex/internal/retrieve"
)

// enrichGraph is the transport wrapper over retrieve.EnrichGraph. It
// converts the wire hit slices to neutral form, delegates the
// neighborhood expansion to retrieve, and maps the neutral GraphResult
// back onto out.Graph. Returns whether anything was emitted — the
// caller uses this to keep avoid/next_action consistent.
func enrichGraph(out *ContextOutput, intent string, view *graphquery.View, semHits []SemHit, symbols []SymbolHit) bool {
	gr, ok := retrieve.EnrichGraph(intent, view, toNeutralSems(semHits), toNeutralSyms(symbols))
	if !ok {
		return false
	}
	wire := &GraphResult{
		Nodes: make([]GraphNode, len(gr.Nodes)),
		Edges: make([]GraphEdge, len(gr.Edges)),
	}
	for i, n := range gr.Nodes {
		wire.Nodes[i] = GraphNode{ID: n.ID, QualifiedName: n.QualifiedName, Kind: n.Kind}
	}
	for i, ed := range gr.Edges {
		wire.Edges[i] = GraphEdge{From: ed.From, To: ed.To, Kind: ed.Kind}
	}
	out.Graph = wire
	return true
}
