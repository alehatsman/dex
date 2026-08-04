package retrieve

import (
	"strconv"

	"github.com/alehatsman/dex/internal/graph"
	"github.com/alehatsman/dex/internal/graphquery"
)

// assembleMaxExpand caps how many call-graph neighbors the assemble intent
// pulls into the inline working set (#723, lever A). Sized so a single ask can
// carry an implementation chain without blowing the byte budget.
const assembleMaxExpand = 24

// roleFormatter renders a SymbolHit.Role from a symbol's raw centrality — the
// vocabulary the transport owns and injects as Assembler.FormatRole. A nil
// formatter yields an empty Role.
type roleFormatter func(name string, inDegree, outDegree, crossPkg int, betweenness float64) string

// inlinePack runs the byte-budget inlining pass natively on the pack (#113):
// widen the assemble pool along the call graph, inline file/symbol bodies,
// account the inlined bytes, and record the #725 completeness signal — all over
// the neutral ContextPack, no wire round-trip. It is the L2 home of the former
// mcp inlineWirePack hook; Assembler.finish calls it directly. A NoInline
// request is a no-op (path-only bundle).
func (a Assembler) inlinePack(req AssembleRequest, pack *ContextPack) {
	if req.NoInline {
		return
	}
	keywords := assembleInlinePrep(req.Intent, req.Graph, pack, req.Candidates.Identifiers, req.Question, a.FormatRole)
	InlineContentKeyed(req.ProjectRoot, req.Intent, pack.SuggestedReads, pack.Symbols, pack.SemanticHits, keywords, a.isTest, a.nonImpl)
	pack.ContentBytesInlined = CountInlinedBytes(pack.SuggestedReads, pack.Symbols, pack.SemanticHits)
	if req.Intent == IntentAssemble {
		pack.Concerns = AssembleConcerns(pack.Symbols, keywords)
	}
}

// isTest applies the injected test-path classifier, defaulting to "nothing is a
// test" when none is wired (transport owns path classification).
func (a Assembler) isTest(p string) bool {
	if a.IsTestPath == nil {
		return false
	}
	return a.IsTestPath(p)
}

// assembleInlinePrep prepares the assemble working set before inlining and
// returns the coverage keys for submodular selection. For non-assemble intents
// it is a pass-through that returns the detected identifiers unchanged. For
// assemble it applies both #723 levers: it widens the symbol pool one hop along
// the call graph (lever A) so the set carries the implementation chain rather
// than just the entrypoint the lanes surfaced, and it derives coverage keys
// from the prose question and the anchor symbol names (lever B) so
// identifier-free or multi-concern queries still engage selection instead of
// degrading to natural order. It mutates pack.Symbols in place with the widened
// pool.
func assembleInlinePrep(intent string, view *graphquery.View, pack *ContextPack, identifiers []string, question string, formatRole roleFormatter) []string {
	if intent != IntentAssemble {
		return identifiers
	}
	pack.Symbols = expandAssemblePool(view, pack.Symbols, pack.SemanticHits, assembleMaxExpand, formatRole)
	anchors := make([]string, len(pack.Symbols))
	for i, s := range pack.Symbols {
		anchors[i] = s.QualifiedName
	}
	return AssembleKeywords(identifiers, question, anchors)
}

// expandAssemblePool grows the assemble symbol working set by one hop along the
// call graph (#723, lever A). For each seed — the symbol-lane hits plus the top
// semantic hits — it resolves the covering graph node and appends that node's
// call-edge neighbors (both callees and callers) as extra SymbolHit candidates.
// This pulls the real implementation chain into the set even when the lanes
// surfaced only an entrypoint or a doc-adjacent chunk. Neighbors are deduped by
// (path, startLine) against the seeds and each other and capped at maxAdd. Seeds
// lead the returned slice so they keep their fields (Signature/Doc/Role/Handle)
// and their priority in coverage selection. A nil view (graph not indexed, or a
// non-Go repo with no call edges) is a no-op, as is maxAdd <= 0. formatRole
// composes the neighbor Role the same way the symbol lane does.
func expandAssemblePool(view *graphquery.View, syms []SymbolHit, sem []SemHit, maxAdd int, formatRole roleFormatter) []SymbolHit {
	if view == nil || maxAdd <= 0 {
		return syms
	}
	loc := func(path string, line int) string { return path + ":" + strconv.Itoa(line) }

	occupied := make(map[string]struct{}, len(syms))
	for _, s := range syms {
		occupied[loc(s.Path, s.StartLine)] = struct{}{}
	}

	// Ordered, deduped seed node IDs — symbol hits first, then semantic hits.
	// Ordered (not a ranged map) so the maxAdd cut is deterministic.
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
	// consider appends neighbor nbID's node as a SymbolHit; returns false once
	// the cap is reached so the caller can stop walking edges.
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
		out = append(out, nodeToSymbolHit(n, formatRole))
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
// preferring the highest-PageRank node when several overlap (the same covering
// rule graphquery.ChunkPageRank uses). ok is false when no node covers the line
// — path absent from the graph, or a gap between nodes.
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
// inliner reads the body from disk by line range regardless. Role is composed
// via the injected formatter (nil → empty) so neighbors look consistent with
// the symbol lane.
func nodeToSymbolHit(n graphquery.Node, formatRole roleFormatter) SymbolHit {
	role := ""
	if formatRole != nil {
		role = formatRole(n.Name, n.InDegree, n.OutDegree, n.CrossPkgCallers, n.Betweenness)
	}
	return SymbolHit{
		QualifiedName: n.QualifiedName,
		Path:          n.FilePath,
		StartLine:     n.StartLine,
		EndLine:       n.EndLine,
		Kind:          string(n.Kind),
		Role:          role,
	}
}
