package mcp

import "github.com/alehatsman/dex/internal/retrieve"

// Bridges the wire-typed lane-agreement feedback (#731) to the domain SemHit
// the assembler carries, so the reweight/shadow machinery keeps its single
// definition on the transport's wire type (avoiding the #734 drift-copy bug)
// while the L2 assembler stays free of the A/B state.

// semLoc keys a semantic hit by its concrete file range — unique per chunk,
// so it recovers a reordering without carrying the wire struct.
type semLoc struct {
	path string
	s, e int
}

// recordShadowPack is the domain-typed adapter for recordShadow (read-only:
// logs the served-vs-shadow delta). Lossy Lanes→names mirrors what the
// former wire pipeline already fed the shadow logger.
func (s *Server) recordShadowPack(intent, question string, sem []retrieve.SemHit) {
	s.recordShadow(intent, question, fromPackSems(sem))
}

// reweightPack is the domain-typed adapter for applyLiveReweight. That call
// is a pure permutation (identity unless DEX_FEEDBACK_LIVE=1), so we run it
// on the wire projection and apply the recovered ordering to the domain
// slice by file-range key — lossless, and a no-op when serving is static.
func (s *Server) reweightPack(intent string, sem []retrieve.SemHit) []retrieve.SemHit {
	reordered := s.applyLiveReweight(intent, fromPackSems(sem))
	if len(reordered) != len(sem) {
		return sem
	}
	idx := make(map[semLoc]int, len(sem))
	for i := range sem {
		idx[semLoc{sem[i].Path, sem[i].StartLine, sem[i].EndLine}] = i
	}
	out := make([]retrieve.SemHit, 0, len(sem))
	for _, w := range reordered {
		i, ok := idx[semLoc{w.Path, w.StartLine, w.EndLine}]
		if !ok {
			return sem // key collision / mismatch — fall back to served order
		}
		out = append(out, sem[i])
	}
	return out
}
