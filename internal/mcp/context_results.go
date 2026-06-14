package mcp

import (
	"github.com/alehatsman/dex/internal/graphquery"
	"github.com/alehatsman/dex/internal/retrieve"
)

// pickSuggestedReads is the transport wrapper over
// retrieve.PickSuggestedReads. It converts the wire hit slices to
// neutral form, delegates the ranking policy to retrieve — injecting
// isNonImplPath, since the transport owns path classification — and maps
// the neutral picks back to wire SuggestedReads. See
// retrieve.PickSuggestedReads for the per-intent strategy.
func pickSuggestedReads(intent string, semHits []SemHit, symbols []SymbolHit, symbolPaths map[string]struct{}, view *graphquery.View) []SuggestedRead {
	picks := retrieve.PickSuggestedReads(intent, toNeutralSems(semHits), toNeutralSyms(symbols), symbolPaths, view, isNonImplPath)
	out := make([]SuggestedRead, len(picks))
	for i := range picks {
		out[i] = SuggestedRead{
			Path:      picks[i].Path,
			StartLine: picks[i].StartLine,
			EndLine:   picks[i].EndLine,
			Reason:    picks[i].Reason,
			Content:   picks[i].Content,
		}
	}
	return out
}
