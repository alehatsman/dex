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
// Assemble runs: symbol lane (+role +non-impl demotion) → semantic lane →
// graph neighborhood → lane-agreement reweight → suggested reads, and
// returns the populated evidence fields of a ContextPack. Enrichment
// (signatures/blame/references/related), inline byte-budgeting, confidence
// and the prose directives remain transport-edge steps for now; later cuts
// move them down behind this same seam.
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
	Intent     string
	Question   string
	Candidates IntentCandidates
	K          int
	Graph      *graphquery.View
	EmbedText  string
	FTSText    string
	Expanded   bool

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

	return pack, AssembleMeta{EmbedFailed: embedFailed}
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
