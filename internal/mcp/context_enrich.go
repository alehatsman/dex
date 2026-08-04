package mcp

import (
	"github.com/alehatsman/dex/internal/retrieve"
)

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
