package mcp

import "github.com/alehatsman/dex/internal/retrieve"

// inlineContent is the transport wrapper over retrieve.InlineContent.
// It converts the wire slices to neutral form, runs the inliner —
// injecting isTestPath, since the transport owns path classification —
// and copies the inline overlay (Content/Body/Truncated/Imports) back
// onto the wire slices in place. See retrieve.InlineContent for the
// per-intent budget and fill model.
func inlineContent(projectRoot, intent string, reads []SuggestedRead, syms []SymbolHit, sem []SemHit) {
	nReads := toNeutralReads(reads)
	nSyms := toNeutralSyms(syms)
	nSem := toNeutralSems(sem)

	retrieve.InlineContent(projectRoot, intent, nReads, nSyms, nSem, isTestPath)

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
