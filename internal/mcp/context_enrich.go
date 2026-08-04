package mcp

import (
	"context"

	"github.com/alehatsman/dex/internal/retrieve"
	"github.com/alehatsman/dex/internal/store"
)

// enrichWire is the transport wrapper over retrieve.Enricher — the same
// wire↔neutral adapter shape inlineContent uses. The enrichment legs
// (signatures/docs, sibling tests, nearest doc, git blame, CODEOWNERS, build
// tags, references, spreading activation) are domain work and live in
// internal/retrieve; this stamps their results back onto the wire
// ContextOutput.
//
// Enrich mutates only Signature/Doc on the symbols (in place, order preserved,
// so the copy-back is index-aligned) and sets References/Annotations/
// RelatedFiles. Nothing else on the wire hits is touched.
func enrichWire(ctx context.Context, root string, st *store.Store, intent string, k int, out *ContextOutput) {
	pk := &retrieve.ContextPack{
		Symbols:        toPackSyms(out.Symbols),
		SuggestedReads: toPackReads(out.SuggestedReads),
	}
	(&retrieve.Enricher{ProjectRoot: root, Store: st, Spread: st}).Enrich(ctx, intent, k, pk)

	for i := range out.Symbols {
		out.Symbols[i].Signature = pk.Symbols[i].Signature
		out.Symbols[i].Doc = pk.Symbols[i].Doc
	}
	out.References = fromNeutralRefs(pk.References)
	out.Annotations = fromNeutralAnnotations(pk.Annotations)
	if pk.RelatedFiles != nil {
		out.RelatedFiles = pk.RelatedFiles
	}
}

// toPackSyms / toPackReads lift the wire hits into the rich pack twins the
// Enricher works over (retrieve.SymbolHit / SuggestedRead — the same shape
// fromPackSyms/fromPackReads project back). Field-explicit so an added wire
// field never silently reaches the enrichment layer.
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

func toPackReads(in []SuggestedRead) []retrieve.SuggestedRead {
	if len(in) == 0 {
		return nil
	}
	out := make([]retrieve.SuggestedRead, len(in))
	for i := range in {
		out[i] = retrieve.SuggestedRead{
			Path:      in[i].Path,
			StartLine: in[i].StartLine,
			EndLine:   in[i].EndLine,
			Reason:    in[i].Reason,
			Content:   in[i].Content,
			Truncated: in[i].Truncated,
			Imports:   in[i].Imports,
		}
	}
	return out
}

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
