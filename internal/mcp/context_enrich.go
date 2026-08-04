package mcp

import (
	"github.com/alehatsman/dex/internal/graphquery"
	"github.com/alehatsman/dex/internal/retrieve"
)

// inlineWirePack is the transport's byte-budget inlining pass, injected into
// retrieve.Assembler as the AssembleRequest.Inline hook. Inlining is
// presentation policy (#725) over wire types — it stays in the transport; this
// wire↔neutral adapter (the same shape enrichWire used) lets the L2 assembler
// drive it at the right point in the sequence.
//
// inlineWorkingSet WIDENS the symbol set along the call graph (assemble intent)
// and stamps Body/Content onto symbols/reads/sem plus Concerns. Symbols change
// count, so they are rebuilt; sem and reads keep their count and order, so only
// the inline overlay (Content/Truncated/Imports) is copied back — preserving
// their Lanes and every other neutral field.
func inlineWirePack(root, intent string, view *graphquery.View, identifiers []string, question string, noInline bool, pk *retrieve.ContextPack) {
	tmp := ContextOutput{
		Symbols:        fromPackSyms(pk.Symbols),
		SemanticHits:   fromPackSems(pk.SemanticHits),
		SuggestedReads: fromPackReads(pk.SuggestedReads),
	}
	inlineWorkingSet(root, intent, view, &tmp, identifiers, question, noInline)

	pk.Symbols = toPackSyms(tmp.Symbols)
	for i := range pk.SemanticHits {
		pk.SemanticHits[i].Content = tmp.SemanticHits[i].Content
		pk.SemanticHits[i].Truncated = tmp.SemanticHits[i].Truncated
	}
	for i := range pk.SuggestedReads {
		pk.SuggestedReads[i].Content = tmp.SuggestedReads[i].Content
		pk.SuggestedReads[i].Truncated = tmp.SuggestedReads[i].Truncated
		pk.SuggestedReads[i].Imports = tmp.SuggestedReads[i].Imports
	}
	pk.ContentBytesInlined = tmp.ContentBytesInlined
	if tmp.Concerns != nil {
		pk.Concerns = retrieve.Concerns{Covered: tmp.Concerns.Covered, Dropped: tmp.Concerns.Dropped}
	} else {
		pk.Concerns = retrieve.Concerns{}
	}
}

// toPackSyms lifts the wire symbols into the rich pack twin the assembler and
// Enricher work over (retrieve.SymbolHit — the shape fromPackSyms projects
// back). Field-explicit so an added wire field never silently reaches L2.
func toPackSyms(in []SymbolHit) []retrieve.SymbolHit {
	if len(in) == 0 {
		return nil
	}
	out := make([]retrieve.SymbolHit, len(in))
	for i := range in {
		out[i] = retrieve.SymbolHit{
			QualifiedName: in[i].QualifiedName,
			Path:          in[i].Path,
			StartLine:     in[i].StartLine,
			EndLine:       in[i].EndLine,
			Kind:          in[i].Kind,
			Signature:     in[i].Signature,
			Doc:           in[i].Doc,
			Body:          in[i].Body,
			Role:          in[i].Role,
			Truncated:     in[i].Truncated,
			Handle:        in[i].Handle,
			SeenTurn:      in[i].SeenTurn,
		}
	}
	return out
}

// fromNeutralRefs / fromNeutralAnnotations project the enrichment results the
// Enricher writes on the pack back onto the wire ContextOutput.
func fromNeutralRefs(in []retrieve.RefHit) []RefHit {
	if len(in) == 0 {
		return nil
	}
	out := make([]RefHit, len(in))
	for i := range in {
		out[i] = RefHit{
			Path:    in[i].Path,
			Line:    in[i].Line,
			Snippet: in[i].Snippet,
			Symbol:  in[i].Symbol,
		}
	}
	return out
}

func fromNeutralAnnotations(in map[string]retrieve.PathMeta) map[string]PathMeta {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]PathMeta, len(in))
	for k, v := range in {
		out[k] = PathMeta{
			LastCommit: v.LastCommit,
			LastAuthor: v.LastAuthor,
			Owners:     v.Owners,
			NearestDoc: v.NearestDoc,
			Tests:      v.Tests,
			BuildTags:  v.BuildTags,
			Package:    v.Package,
		}
	}
	return out
}
