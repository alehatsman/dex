package graph

// Shared infrastructure for query-driven ("tags") graph extractors. A
// tags extractor replaces a hand-rolled recursive descent with a single
// tree-sitter query that enumerates every definition / call / import in
// the file; scope (method-of-class, caller-of-call, nesting) is then
// recovered by walking each matched node's ancestors. The per-language
// resolution layer (import table / symbol table / resolveCall / Finalize)
// is reused verbatim — tags-queries are name-resolved discovery only, so
// the graph (and trace score) is identical to the walker's.
//
// This file holds the language-agnostic primitives: the query runner and
// the ancestor-walk helpers used to reconstruct scope. Each language ships
// a query string plus a thin strategy that maps captures to the existing
// emit/resolve code.

import (
	sitter "github.com/smacker/go-tree-sitter"
)

// runTagsQuery executes q over root and invokes onCapture for every
// capture, in match order. The caller compiles and caches q (compilation
// is per-grammar, not per-file). Capture order does not matter to the
// resulting graph: node/edge IDs are content-addressed and call
// resolution is deferred to Finalize, so emitting a symbol before or
// after recording a reference to it is equivalent.
func runTagsQuery(q *sitter.Query, root *sitter.Node, onCapture func(capture string, n *sitter.Node)) {
	qc := sitter.NewQueryCursor()
	defer qc.Close()
	qc.Exec(q, root)
	for {
		m, ok := qc.NextMatch()
		if !ok {
			return
		}
		for _, c := range m.Captures {
			onCapture(q.CaptureNameForId(c.Index), c.Node)
		}
	}
}

// hasAncestorOfType reports whether any ancestor of n has one of the given
// node types. Used to reject definitions/calls the recursive walker would
// never have reached (e.g. a def nested inside another function).
func hasAncestorOfType(n *sitter.Node, types ...string) bool {
	return firstAncestorOfType(n, types...) != nil
}

// firstAncestorOfType returns the closest ancestor of n whose type is one
// of types, or nil. The match is by proximity: the first matching node
// encountered walking parent links upward wins.
func firstAncestorOfType(n *sitter.Node, types ...string) *sitter.Node {
	for p := n.Parent(); p != nil; p = p.Parent() {
		for _, t := range types {
			if p.Type() == t {
				return p
			}
		}
	}
	return nil
}

// nodeContains reports whether outer's byte range encloses inner's start.
// Used to confirm a call sits in a function's body subtree rather than its
// parameter list / decorators / default-argument expressions.
func nodeContains(outer, inner *sitter.Node) bool {
	if outer == nil || inner == nil {
		return false
	}
	return inner.StartByte() >= outer.StartByte() && inner.StartByte() < outer.EndByte()
}
