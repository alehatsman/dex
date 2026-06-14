package mcp

import "github.com/alehatsman/dex/internal/retrieve"

// ─── next_action / avoid (prose) ──────────────────────────────────────────
//
// The prose builders live in internal/retrieve (BuildNextAction /
// BuildAvoid) over neutral types. These thin wrappers convert the wire
// hit slices to neutral and delegate. hasBlameAnnotations stays here —
// it reads the wire PathMeta annotations and feeds BuildNextAction a
// plain bool.

// buildNextAction is the transport wrapper over retrieve.BuildNextAction.
func buildNextAction(intent string, reads []SuggestedRead, symbols []SymbolHit, topSemScore float32, graphEdgeCount, refCount int, hasBlame bool) string {
	return retrieve.BuildNextAction(intent, toNeutralReads(reads), toNeutralSyms(symbols), topSemScore, graphEdgeCount, refCount, hasBlame)
}

// buildAvoid is the transport wrapper over retrieve.BuildAvoid.
func buildAvoid(intent string, semHits []SemHit, symbols []SymbolHit, graphIndexed, hasRefs bool) string {
	return retrieve.BuildAvoid(intent, toNeutralSems(semHits), toNeutralSyms(symbols), graphIndexed, hasRefs)
}

// hasBlameAnnotations reports whether any path in the annotations map
// carries blame metadata — the signal that BuildNextAction uses to
// avoid emitting "weak match" on editing_context responses that have
// concrete authorship data.
func hasBlameAnnotations(anns map[string]PathMeta) bool {
	for _, m := range anns {
		if m.LastCommit != "" || m.LastAuthor != "" {
			return true
		}
	}
	return false
}

// ─── inline helpers ───────────────────────────────────────────────────────

// countInlinedBytes sums len(Content) / len(Body) across the three output lanes.
func countInlinedBytes(reads []SuggestedRead, syms []SymbolHit, sem []SemHit) int {
	return retrieve.CountInlinedBytes(toNeutralReads(reads), toNeutralSyms(syms), toNeutralSems(sem))
}
