package mcp

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/alehatsman/dex/internal/graph"
	"github.com/alehatsman/dex/internal/graphquery"
	"github.com/alehatsman/dex/internal/retrieve"
)

// assembleInlinePrep prepares the assemble working set before inlining and
// returns the coverage keys for submodular selection. For non-assemble
// intents it is a pass-through that returns the detected identifiers
// unchanged. For assemble it applies both #723 levers: it widens the symbol
// pool one hop along the call graph (lever A) so the set carries the
// implementation chain rather than just the entrypoint the lanes surfaced,
// and it derives coverage keys from the prose question and the anchor symbol
// names (lever B) so identifier-free or multi-concern queries still engage
// selection instead of degrading to natural order. It mutates out.Symbols in
// place with the widened pool.
func assembleInlinePrep(intent string, view *graphquery.View, out *ContextOutput, identifiers []string, question string) []string {
	if intent != retrieve.IntentAssemble {
		return identifiers
	}
	out.Symbols = expandAssemblePool(view, out.Symbols, out.SemanticHits, assembleMaxExpand)
	anchors := make([]string, len(out.Symbols))
	for i, s := range out.Symbols {
		anchors[i] = s.QualifiedName
	}
	return retrieve.AssembleKeywords(identifiers, question, anchors)
}

// expandAssemblePool grows the assemble symbol working set by one hop along
// the call graph (#723, lever A). For each seed — the symbol-lane hits plus
// the top semantic hits — it resolves the covering graph node and appends
// that node's call-edge neighbors (both callees and callers) as extra
// SymbolHit candidates. This pulls the real implementation chain into the
// set even when the lanes surfaced only an entrypoint or a doc-adjacent
// chunk. Neighbors are deduped by (path, startLine) against the seeds and
// each other and capped at maxAdd. Seeds lead the returned slice so they
// keep their fields (Signature/Doc/Role/Handle) and their priority in
// coverage selection. A nil view (graph not indexed, or a non-Go repo with
// no call edges) is a no-op, as is maxAdd <= 0.
func expandAssemblePool(view *graphquery.View, syms []SymbolHit, sem []SemHit, maxAdd int) []SymbolHit {
	if view == nil || maxAdd <= 0 {
		return syms
	}
	loc := func(path string, line int) string { return path + ":" + strconv.Itoa(line) }

	occupied := make(map[string]struct{}, len(syms))
	for _, s := range syms {
		occupied[loc(s.Path, s.StartLine)] = struct{}{}
	}

	// Ordered, deduped seed node IDs — symbol hits first, then semantic
	// hits. Ordered (not a ranged map) so the maxAdd cut is deterministic.
	var seedIDs []string
	seedSeen := map[string]struct{}{}
	pushSeed := func(path string, line int) {
		n, ok := coveringNode(view, path, line)
		if !ok {
			return
		}
		if _, dup := seedSeen[n.ID]; dup {
			return
		}
		seedSeen[n.ID] = struct{}{}
		seedIDs = append(seedIDs, n.ID)
	}
	for _, s := range syms {
		pushSeed(s.Path, s.StartLine)
	}
	for _, h := range sem {
		pushSeed(h.Path, h.StartLine)
	}

	out := syms
	added := 0
	// consider appends neighbor nbID's node as a SymbolHit; returns false
	// once the cap is reached so the caller can stop walking edges.
	consider := func(nbID string) bool {
		if added >= maxAdd {
			return false
		}
		n, ok := view.NodesByID[nbID]
		if !ok || n.FilePath == "" {
			return true
		}
		k := loc(n.FilePath, n.StartLine)
		if _, dup := occupied[k]; dup {
			return true
		}
		occupied[k] = struct{}{}
		out = append(out, nodeToSymbolHit(n))
		added++
		return added < maxAdd
	}

	for _, id := range seedIDs {
		for _, e := range view.EdgesBySrc[id] { // callees
			if e.Kind == graph.EdgeCalls && !consider(e.DstID) {
				return out
			}
		}
		for _, e := range view.EdgesByDst[id] { // callers
			if e.Kind == graph.EdgeCalls && !consider(e.SrcID) {
				return out
			}
		}
	}
	return out
}

// coveringNode returns the graph node in path whose line range covers line,
// preferring the highest-PageRank node when several overlap (the same
// covering rule graphquery.ChunkPageRank uses). ok is false when no node
// covers the line — path absent from the graph, or a gap between nodes.
func coveringNode(view *graphquery.View, path string, line int) (graphquery.Node, bool) {
	var best graphquery.Node
	found := false
	for _, n := range view.NodesByPath[path] {
		if line >= n.StartLine && line <= n.EndLine {
			if !found || n.PageRank > best.PageRank {
				best, found = n, true
			}
		}
	}
	return best, found
}

// nodeToSymbolHit projects a graph node into a SymbolHit candidate for the
// assemble pool. Signature is left empty (graph nodes don't carry one); the
// inliner reads the body from disk by line range regardless. Role is
// composed the same way as the symbol lane so neighbors look consistent.
func nodeToSymbolHit(n graphquery.Node) SymbolHit {
	return SymbolHit{
		QualifiedName: n.QualifiedName,
		Path:          n.FilePath,
		StartLine:     n.StartLine,
		EndLine:       n.EndLine,
		Kind:          string(n.Kind),
		Role:          formatRole(n.Name, n.InDegree, n.OutDegree, n.CrossPkgCallers, n.Betweenness),
	}
}

// inlineWorkingSet inlines file/symbol bodies into the bundle (unless the
// caller opted out) and, for assemble, records the #725 completeness signal
// from the same coverage keys the inliner selected on. Split out of
// contextRouterStream to keep that router under the cyclop cap.
func inlineWorkingSet(root, intent string, view *graphquery.View, out *ContextOutput, identifiers []string, question string, noInline bool) {
	if noInline {
		return
	}
	keywords := assembleInlinePrep(intent, view, out, identifiers, question)
	inlineContent(root, intent, out.SuggestedReads, out.Symbols, out.SemanticHits, keywords)
	out.ContentBytesInlined = countInlinedBytes(out.SuggestedReads, out.Symbols, out.SemanticHits)
	if intent == retrieve.IntentAssemble {
		out.Concerns = assembleConcerns(out.Symbols, keywords)
	}
}

// assembleConcerns computes the completeness signal for an assemble working
// set (#725): a concern keyword is COVERED when some symbol whose body was
// actually inlined is about it — its qualified name or signature contains the
// keyword, the same name+signature haystack the submodular selector scored —
// and DROPPED otherwise. Returns nil when there are no coverage keys.
//
// Coverage is judged on name+signature, not body text: a stem like "store"
// occurs in countless bodies, but a symbol is only ABOUT a concern when the
// concern is in what it is named or declared. This mirrors the selection
// (retrieve.coverageOrder) so the signal matches the set it explains. A
// dropped concern means the byte budget left no symbol about it in the set —
// the working set is partial, not a false floor.
func assembleConcerns(syms []SymbolHit, keywords []string) *AssembleConcerns {
	if len(keywords) == 0 {
		return nil
	}
	covered := make(map[string]bool, len(keywords))
	for i := range syms {
		if syms[i].Body == "" {
			continue // not inlined → not in the working set
		}
		hay := strings.ToLower(syms[i].QualifiedName + " " + syms[i].Signature)
		for _, k := range keywords {
			if !covered[k] && strings.Contains(hay, k) { // keywords are pre-lowercased
				covered[k] = true
			}
		}
	}
	out := &AssembleConcerns{}
	for _, k := range keywords {
		if covered[k] {
			out.Covered = append(out.Covered, k)
		} else {
			out.Dropped = append(out.Dropped, k)
		}
	}
	return out
}

// assembleNextActionHint augments next_action with the #725/#729 signals:
//   - editing_context with a multi-site shape → nudge toward intent=assemble,
//     which batches the symbol bodies for the change in one call (serves the
//     "batch reads" instinct without the agent knowing the knob exists).
//   - assemble with dropped concerns → caveat that the set is partial, so an
//     honest partial isn't mistaken for a complete answer. When the set has a
//     covered anchor to chain from, the caveat upgrades to a concrete graph
//     move ("trace callees of <anchor> …") so the agent is handed the next
//     command, not just told the set is incomplete (#729).
func assembleNextActionHint(intent, next string, concerns *AssembleConcerns, nReads int, syms []SymbolHit) string {
	switch intent {
	case retrieve.IntentEditingContext:
		if nReads+len(syms) > 1 {
			return strings.TrimSpace(next +
				" To pull the full working set of symbol bodies for this change in one call, re-run with intent=assemble.")
		}
	case retrieve.IntentAssemble:
		if concerns != nil && len(concerns.Dropped) > 0 {
			total := len(concerns.Covered) + len(concerns.Dropped)
			if anchor := firstInlinedAnchor(syms); anchor != "" {
				// Concrete chained directive: the dropped concerns are likely
				// one hop out from a symbol already in the set (same lever-A
				// call-graph logic expandAssemblePool widens on), so name the
				// anchor and the move that reaches them (#729).
				return strings.TrimSpace(fmt.Sprintf("%s Working set covers %d of %d concerns — DROPPED %v. Trace callees of %s (or raise k) to pull them into the set. Do not treat this set as complete.",
					next, len(concerns.Covered), total, concerns.Dropped, anchor))
			}
			// No covered anchor to chain from (e.g. nsyms=0 pure-prose miss):
			// keep the generic caveat — there's nothing in the set to trace
			// from. That prose gap is upstream retrieval (#687/#723), not here.
			return strings.TrimSpace(fmt.Sprintf("%s Working set covers %d of %d concerns — DROPPED %v have no symbol body here; narrow the question to them, raise k, or read them directly. Do not treat this set as complete.",
				next, len(concerns.Covered), total, concerns.Dropped))
		}
	}
	return next
}

// firstInlinedAnchor returns the qualified name of the first symbol whose body
// was actually inlined into the assemble set, so a chained "trace callees of
// <anchor>" directive points at a symbol that is really in the working set.
// Empty when nothing was inlined (the pure-prose miss where nsyms=0).
func firstInlinedAnchor(syms []SymbolHit) string {
	for i := range syms {
		if syms[i].Body != "" {
			return syms[i].QualifiedName
		}
	}
	return ""
}
