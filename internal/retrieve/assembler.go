package retrieve

import (
	"context"
	"sort"

	"github.com/alehatsman/dex/internal/graphquery"
	"github.com/alehatsman/dex/internal/store"
)

// Assembler composes the evidence lanes into a ContextPack — the L2 domain
// core of `ask` assembly (#95a / #103). It holds the retrieval backends
// (via Service); the per-request policy the transport deliberately owns —
// the call-graph role vocabulary and path classification, both shared with
// the search/symbol/graph tools — is injected as funcs rather than moved
// down, per the design note on Service.SymHit.
//
// Assemble runs the whole `ask` domain sequence: symbol lane (+role +non-impl
// demotion) → semantic lane → graph neighborhood → lane-agreement reweight →
// suggested reads → inline byte-budgeting (injected presentation hook) →
// enrichment (signatures/blame/references/related) → confidence and the prose
// directives. It returns a complete ContextPack; the transport router only
// projects it and applies transport concerns (session/throttle/answer/dedup).
type Assembler struct {
	// Service is the query-time retrieval engine (embedder + lanes).
	Service Service

	// FormatRole renders SymbolHit.Role from a symbol's raw centrality
	// columns. Injected by the transport, which owns the role-display
	// vocabulary shared across tools. nil yields empty roles.
	FormatRole func(name string, inDegree, outDegree, crossPkg int, betweenness float64) string

	// IsNonImpl reports whether a path is non-implementation (test/doc/
	// build/fixture) — the demotion tiebreaker that keeps the prose
	// directive on real code. Injected (transport owns path classification).
	// nil treats every path as implementation.
	IsNonImpl func(string) bool
}

// AssembleRequest carries the resolved per-request inputs the evidence core
// needs. Intent/Candidates come from ResolveIntent; EmbedText/FTSText from
// the expansion step; Graph is the per-request view (nil = none indexed).
type AssembleRequest struct {
	Intent      string
	Question    string
	Candidates  IntentCandidates
	K           int
	Graph       *graphquery.View
	EmbedText   string
	FTSText     string
	Expanded    bool
	ProjectRoot string          // repo root for the enrichment file/git legs
	NoInline    bool            // caller opted out of body inlining
	Spread      store.Spreader  // optional; nil = no spreading-activation related files

	// Inline is the byte-budget inlining pass (#725 presentation policy). It
	// widens the assemble working set along the call graph and stamps
	// Body/Content/Concerns onto the pack. Transport-owned (it is presentation,
	// not retrieval) and injected as a hook; nil skips inlining.
	Inline func(pack *ContextPack)

	// Reweight, when non-nil, reorders the semantic hits (lane-agreement
	// feedback, #731) after graph enrichment. RecordShadow logs the
	// static-vs-reweighted pair for regression monitoring. Both are
	// transport-owned (server feedback state) and injected so the domain
	// core stays free of the A/B machinery.
	Reweight     func(intent string, sem []SemHit) []SemHit
	RecordShadow func(intent, question string, sem []SemHit)
}

// AssembleMeta carries the non-pack signals the transport still needs at the
// edge: EmbedFailed drives the "embed offline" hint and endpoint surfacing.
type AssembleMeta struct {
	EmbedFailed bool
}

// Assemble builds the evidence core of a ContextPack. It is behavior-neutral
// with the former inline mcp pipeline: same lane order, same reweight point,
// same demotion, same graph-before-reweight-before-pick sequence.
func (a Assembler) Assemble(ctx context.Context, st store.Searcher, req AssembleRequest) (ContextPack, AssembleMeta) {
	pack := ContextPack{Intent: req.Intent, Question: req.Question, Expanded: req.Expanded}

	// Symbol lane — exact identifier lookups. Demote test/doc/build/fixture
	// paths (stable, by path only) so the prose directive lands on real
	// implementation; identical to the former wire-side demotion.
	rawSyms, symbolPaths := a.Service.SymbolLane(ctx, st, req.Candidates, req.K)
	sort.SliceStable(rawSyms, func(i, j int) bool {
		return !a.nonImpl(rawSyms[i].Path) && a.nonImpl(rawSyms[j].Path)
	})
	pack.Symbols = a.toSymbolHits(rawSyms)

	// Semantic lane — runs unless embed is offline.
	sems, embedFailed := a.Service.SemanticLane(ctx, st, req.EmbedText, req.FTSText, req.K)
	pack.SemanticHits = sems

	// Graph neighborhood — computed over the pre-reweight order (lane
	// provenance is final post-enrich).
	if gr, ok := EnrichGraph(req.Intent, req.Graph, sems, rawSyms); ok {
		pack.Graph = gr
	}

	// Lane-agreement reweight (#731): shadow-log, then serve the reweighted
	// order when the transport's live flag is on.
	if req.RecordShadow != nil {
		req.RecordShadow(req.Intent, req.Question, sems)
	}
	if req.Reweight != nil {
		sems = req.Reweight(req.Intent, sems)
		pack.SemanticHits = sems
	}

	pack.SuggestedReads = PickSuggestedReads(req.Intent, sems, rawSyms, symbolPaths, req.Graph, a.nonImpl)

	// No lane produced anything: skip the tail (it would be a no-op on the
	// evidence, but the prose builders always emit a directive — the empty-
	// result messaging is the transport's noLaneHits responsibility). This
	// gate matches noLaneHits' hit check exactly, keeping output byte-neutral.
	if len(pack.Symbols) == 0 && len(pack.SemanticHits) == 0 {
		return pack, AssembleMeta{EmbedFailed: embedFailed}
	}

	a.finish(ctx, st, req, &pack)
	return pack, AssembleMeta{EmbedFailed: embedFailed}
}

// finish runs the post-evidence tail on the pack: inline (injected) →
// enrichment → confidence + prose. Ordering is load-bearing: inline widens the
// working set and computes Concerns over the pre-enrich signatures; enrichment
// then fills signatures/refs/blame; the prose reads both. Mirrors the former
// edge sequence exactly.
func (a Assembler) finish(ctx context.Context, st store.Searcher, req AssembleRequest, pack *ContextPack) {
	if req.Inline != nil {
		req.Inline(pack)
	}

	(&Enricher{ProjectRoot: req.ProjectRoot, Store: st, Spread: req.Spread}).
		Enrich(ctx, req.Intent, req.K, pack)

	topSem := maxSemScore(pack.SemanticHits)
	graphEdges := 0
	if pack.Graph != nil {
		graphEdges = len(pack.Graph.Edges)
	}
	hasBlame := hasBlameMeta(pack.Annotations)
	syms := packSymHits(pack.Symbols)

	pack.NextAction = BuildNextAction(req.Intent, pack.SuggestedReads, syms, topSem, graphEdges, len(pack.References), hasBlame)
	pack.Confidence = ConfidenceLevel(req.Intent, len(pack.Symbols), topSem, graphEdges, hasBlame)
	// #725: nudge edit-intent toward assemble, and caveat a partial assemble set.
	pack.NextAction = AssembleNextActionHint(req.Intent, pack.NextAction, pack.Concerns, len(pack.SuggestedReads), syms)
	// If the directive's primary read was truncated at inline time, flag that so
	// the agent knows the inlined Content isn't the full chunk.
	if !req.NoInline && len(pack.SuggestedReads) > 0 && pack.SuggestedReads[0].Truncated {
		pack.NextAction += " The inlined content is truncated at inline-budget caps — Read the full line range if you need the tail."
	}
	pack.Avoid = BuildAvoid(req.Intent, pack.SemanticHits, syms, req.Graph != nil, len(pack.References) > 0)
}

// maxSemScore returns the top semantic Score (hits aren't strictly sorted —
// summary merging and rerank permute them). Mirrors mcp.maxSemanticScore.
func maxSemScore(hits []SemHit) float32 {
	var top float32
	for _, h := range hits {
		if h.Score > top {
			top = h.Score
		}
	}
	return top
}

// hasBlameMeta reports whether any annotation carries git-blame data — the
// signal the prose uses to claim editing provenance. Mirrors
// mcp.hasBlameAnnotations over the neutral PathMeta.
func hasBlameMeta(anns map[string]PathMeta) bool {
	for _, m := range anns {
		if m.LastCommit != "" || m.LastAuthor != "" {
			return true
		}
	}
	return false
}

// packSymHits projects the rich pack SymbolHit onto the lean SymHit the prose
// builders (BuildNextAction/BuildAvoid/AssembleConcerns) read — name, path and
// the inlined body/signature that coverage and anchors key on.
func packSymHits(in []SymbolHit) []SymHit {
	if len(in) == 0 {
		return nil
	}
	out := make([]SymHit, len(in))
	for i := range in {
		out[i] = SymHit{
			QualifiedName: in[i].QualifiedName,
			Path:          in[i].Path,
			StartLine:     in[i].StartLine,
			EndLine:       in[i].EndLine,
			Kind:          in[i].Kind,
			Signature:     in[i].Signature,
			Body:          in[i].Body,
			Truncated:     in[i].Truncated,
		}
	}
	return out
}

// nonImpl applies the injected classifier, defaulting to "everything is
// implementation" when none is wired.
func (a Assembler) nonImpl(p string) bool {
	if a.IsNonImpl == nil {
		return false
	}
	return a.IsNonImpl(p)
}

// toSymbolHits maps neutral lane rows to the rich pack SymbolHit, formatting
// the Role via the injected formatter. Doc/Body/Handle/SeenTurn/Truncated
// stay zero here — enrichment, inline and edge stamping fill them later.
func (a Assembler) toSymbolHits(raw []SymHit) []SymbolHit {
	if raw == nil {
		return nil
	}
	out := make([]SymbolHit, 0, len(raw))
	for _, h := range raw {
		role := ""
		if a.FormatRole != nil {
			role = a.FormatRole(h.Name, h.InDegree, h.OutDegree, h.CrossPkgCallers, h.Betweenness)
		}
		out = append(out, SymbolHit{
			QualifiedName: h.QualifiedName,
			Path:          h.Path,
			StartLine:     h.StartLine,
			EndLine:       h.EndLine,
			Kind:          h.Kind,
			Signature:     h.Signature,
			Role:          role,
		})
	}
	return out
}
