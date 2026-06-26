package mcp

import "github.com/alehatsman/dex/internal/retrieve"

// inlineContent is the transport wrapper over retrieve.InlineContent.
// It converts the wire slices to neutral form, runs the inliner —
// injecting isTestPath, since the transport owns path classification —
// and copies the inline overlay (Content/Body/Truncated/Imports) back
// onto the wire slices in place. See retrieve.InlineContent for the
// per-intent budget and fill model.
// keywords carries the query identifiers the assemble intent (#687) needs for
// submodular symbol selection; it is ignored for every other intent (pass nil).
func inlineContent(projectRoot, intent string, reads []SuggestedRead, syms []SymbolHit, sem []SemHit, keywords []string) {
	nReads := toNeutralReads(reads)
	nSyms := toNeutralSyms(syms)
	nSem := toNeutralSems(sem)

	retrieve.InlineContentKeyed(projectRoot, intent, nReads, nSyms, nSem, keywords, isTestPath)

	for i := range reads {
		reads[i].Content = nReads[i].Content
		reads[i].Truncated = nReads[i].Truncated
		reads[i].Imports = nReads[i].Imports
	}
	for i := range syms {
		syms[i].Body = nSyms[i].Body
		syms[i].Truncated = nSyms[i].Truncated
	}
	for i := range sem {
		sem[i].Content = nSem[i].Content
		sem[i].Truncated = nSem[i].Truncated
	}
}
