package mcp

import (
	"fmt"
	"strings"

	"github.com/alehatsman/dex/internal/retrieve"
)

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

// reconcileNextActionWithAnswer keeps next_action from contradicting the
// synthesized answer it ships with. next_action is built deterministically
// from suggested_reads[0] (top raw semantic score) BEFORE the answer is
// synthesized; the answer is reranked + LLM-led, so on a near-tie the two
// can name different "primary" files — the agent then reads the loser
// next_action points at while the answer (correct) leads elsewhere (#532).
//
// Surgical: only rewrites when next_action currently points at the primary
// read AND the answer leads with a *different* file that is itself a
// suggested read. Graph/edge directives (which name no read path) and
// already-consistent directives are left untouched.
func reconcileNextActionWithAnswer(out *ContextOutput) {
	if strings.TrimSpace(out.Answer) == "" || strings.TrimSpace(out.NextAction) == "" || len(out.SuggestedReads) == 0 {
		return
	}
	lead := firstAnswerLeadPath(out.Answer)
	primary := out.SuggestedReads[0]
	if lead == "" || lead == primary.Path {
		return // no citation, or already consistent
	}
	// Only touch directives that actually point at the current primary read.
	if !strings.Contains(out.NextAction, primary.Path) {
		return
	}
	// The answer's lead must itself be a suggested read (else it's an
	// ungrounded citation — handled separately by #526's path validation).
	var leadRead *SuggestedRead
	for i := range out.SuggestedReads {
		if out.SuggestedReads[i].Path == lead {
			leadRead = &out.SuggestedReads[i]
			break
		}
	}
	if leadRead == nil {
		return
	}
	out.NextAction = fmt.Sprintf(
		"Read %s lines %d-%d first — the synthesized answer leads there, not %s.",
		leadRead.Path, leadRead.StartLine, leadRead.EndLine, primary.Path)
	if leadRead.Truncated {
		out.NextAction += " The inlined content is truncated — Read the full line range if you need the tail."
	}
}

// firstAnswerLeadPath returns the first repo-relative file path cited in the
// synthesized answer (line suffix stripped), or "" if none. Reuses the
// citation regex from the answer path-validation lane (#526).
func firstAnswerLeadPath(answer string) string {
	m := answerPathCitation.FindStringSubmatch(answer)
	if len(m) < 2 {
		return ""
	}
	return m[1]
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
