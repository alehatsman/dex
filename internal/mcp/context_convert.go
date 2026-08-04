package mcp

import "github.com/alehatsman/dex/internal/retrieve"

// Wire ↔ neutral conversion for the composition step. The algorithms
// live in internal/retrieve over transport-free types (no json tags,
// no #344 handle/seen-turn fields, which the transport stamps at the
// edge). These adapters convert the wire structs to neutral for the
// retrieve calls and copy the inline overlay (Content/Body/Truncated
// /Imports) back onto the wire slices afterward. They are intentionally
// field-explicit so an added wire field doesn't silently leak into the
// retrieval layer.

func toNeutralSems(in []SemHit) []retrieve.SemHit {
	out := make([]retrieve.SemHit, len(in))
	for i := range in {
		out[i] = retrieve.SemHit{
			Path:      in[i].Path,
			StartLine: in[i].StartLine,
			EndLine:   in[i].EndLine,
			Kind:      in[i].Kind,
			Score:     in[i].Score,
			Reason:    in[i].Reason,
			Content:   in[i].Content,
			Truncated: in[i].Truncated,
		}
	}
	return out
}

func toNeutralSyms(in []SymbolHit) []retrieve.SymbolHit {
	out := make([]retrieve.SymbolHit, len(in))
	for i := range in {
		out[i] = retrieve.SymbolHit{
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

func toNeutralReads(in []SuggestedRead) []retrieve.SuggestedRead {
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
