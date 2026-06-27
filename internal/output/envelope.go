// Package output defines the shared, machine-readable output envelope that dex
// verbs embed into their JSON / MCP StructuredOutput. It is a zero-internal-dep
// leaf: it must never import another dex package, so that both the CLI
// (cmd/dex) and the MCP server (internal/mcp) can embed the contract without an
// import cycle. The MCP layer *adapts* this contract; it does not own it.
//
// The envelope is composed of small, embeddable pieces (Confidence,
// EvidenceSpan, StaleStatus, NextCall, TokenEstimate) carried by Envelope.
// Embedding Envelope into an existing verb output struct promotes those fields
// into its JSON without a rewrite.
package output

import (
	"fmt"
	"time"
)

// Level is the confidence level — a coarse enum, deliberately NOT a numeric
// score: a consumer should reason about high/medium/low, not chase decimals.
type Level string

const (
	// LevelHigh — the result rests on exact, line-level evidence.
	LevelHigh Level = "high"
	// LevelMedium — the result is well-supported but with a known gap.
	LevelMedium Level = "medium"
	// LevelLow — the result is best-effort / degraded (no graph, no index, …).
	LevelLow Level = "low"
)

// Confidence reports how much to trust a result and why. Level is always set;
// Basis lists the signals that support it; Gaps names what is missing (e.g.
// "no line-level span available"). A backend that cannot produce spans explains
// itself here rather than fabricating evidence.
type Confidence struct {
	Level Level    `json:"level"`
	Basis []string `json:"basis,omitempty"`
	Gaps  []string `json:"gaps,omitempty"`
}

// SpanKind labels whether an EvidenceSpan's line range is exact (resolved from
// the index / parse) or inferred (a heuristic guess). Never label an inferred
// span exact.
type SpanKind string

const (
	// SpanExact — start/end are the true bounds of the cited symbol/range.
	SpanExact SpanKind = "exact"
	// SpanInferred — start/end are a heuristic approximation.
	SpanInferred SpanKind = "inferred"
)

// EvidenceSpan is one line-level citation backing a result. When an item is
// present its Path/Start/End are mandatory — they locate the evidence. An empty
// evidence list ([]) is a valid state (the result has no line-level span); in
// that case the reason is explained in Confidence.Gaps. NEVER fabricate a span
// to fill the slot.
type EvidenceSpan struct {
	Path   string   `json:"path"`
	Start  int      `json:"start"`
	End    int      `json:"end"`
	Symbol string   `json:"symbol,omitempty"`
	Kind   SpanKind `json:"kind,omitempty"`
	Reason string   `json:"reason,omitempty"`
}

// Coverage names how thoroughly staleness was checked. v1 of the envelope only
// computes age-based staleness (CoverageAgeOnly); the commit/dirty tiers are
// reserved for M1.5 (#819) when indexed-commit vs worktree-commit detection
// lands. CoverageUnknown means staleness was not checked at all.
type Coverage string

const (
	CoverageUnknown       Coverage = "unknown"
	CoverageAgeOnly       Coverage = "age_only"
	CoverageCommitOnly    Coverage = "commit_only"
	CoverageDirtyWorktree Coverage = "dirty_worktree"
	CoverageFull          Coverage = "full"
)

// StaleStatus reports whether the index a result was drawn from may be out of
// date. In v1 only the age-based fields are populated (Coverage == age_only):
// IndexedCommit/WorktreeCommit/DirtyPaths ship empty until M1.5 (#819) wires
// git-state detection. is_stale flips on the existing 24h-age heuristic.
type StaleStatus struct {
	IsStale        bool     `json:"is_stale"`
	Coverage       Coverage `json:"coverage"`
	LastIndexedAt  string   `json:"last_indexed_at,omitempty"`
	IndexedCommit  string   `json:"indexed_commit,omitempty"`
	WorktreeCommit string   `json:"worktree_commit,omitempty"`
	DirtyPaths     []string `json:"dirty_paths,omitempty"`
}

// StaleAgeThreshold is the age past which an index is considered stale by the
// age-only heuristic. It mirrors contextRouterCheckStale's 24h window so the
// envelope and the ask hint agree.
const StaleAgeThreshold = 24 * time.Hour

// AgeStale computes an age-only StaleStatus from the index's last-indexed time.
// A zero lastIndexed (never indexed / unknown) yields Coverage=unknown with
// is_stale=false — staleness was simply not determinable. This is the only
// staleness computation in v1; it is pure (no store/git dependency) so the leaf
// package stays dep-free.
func AgeStale(lastIndexed time.Time) StaleStatus {
	if lastIndexed.IsZero() {
		return StaleStatus{Coverage: CoverageUnknown}
	}
	return StaleStatus{
		IsStale:       time.Since(lastIndexed) > StaleAgeThreshold,
		Coverage:      CoverageAgeOnly,
		LastIndexedAt: lastIndexed.UTC().Format(time.RFC3339),
	}
}

// NextCall is a concrete suggested follow-up: the tool to call, its arguments,
// and why it helps. Reason is mandatory — a suggestion without a rationale is
// noise. The producer caps the list at MaxNextCalls.
type NextCall struct {
	Tool   string `json:"tool"`
	Args   string `json:"args,omitempty"`
	Reason string `json:"reason"`
}

// MaxNextCalls is the hard cap on suggested follow-ups: more than three is a
// menu, not a recommendation.
const MaxNextCalls = 3

// TokenEstimate compares the raw cost of the underlying material against the
// packed cost of what the verb actually returned. Populated by the verbs that
// do budget-bounded packing (M3 assemble); left nil elsewhere in v1.
type TokenEstimate struct {
	RawTokens    int `json:"raw_tokens"`
	PackedTokens int `json:"packed_tokens"`
}

// Envelope is the embeddable cross-cutting contract. Embed it (anonymously)
// into a verb's JSON output struct to promote these fields alongside the verb's
// own. Confidence and Stale are always present (a result always has *some*
// confidence and *some* staleness coverage, even if unknown); Evidence is
// always present but may be empty []; RiskFlags / NextCalls / TokenEstimate are
// optional. Call Normalize before marshalling so an unset Evidence renders as
// [] rather than null.
type Envelope struct {
	Confidence    Confidence     `json:"confidence"`
	Evidence      []EvidenceSpan `json:"evidence"`
	Stale         StaleStatus    `json:"stale"`
	RiskFlags     []string       `json:"risk_flags,omitempty"`
	NextCalls     []NextCall     `json:"next_calls,omitempty"`
	TokenEstimate *TokenEstimate `json:"token_estimate,omitempty"`
}

// Normalize makes the envelope safe to marshal: a nil Evidence becomes an empty
// slice (so JSON emits [] not null), an unset Coverage becomes unknown, and the
// NextCalls list is truncated to the cap. Idempotent.
func (e *Envelope) Normalize() {
	if e.Evidence == nil {
		e.Evidence = []EvidenceSpan{}
	}
	if e.Stale.Coverage == "" {
		e.Stale.Coverage = CoverageUnknown
	}
	if len(e.NextCalls) > MaxNextCalls {
		e.NextCalls = e.NextCalls[:MaxNextCalls]
	}
}

// Validate checks the envelope's invariants. It is used by tests (and may be
// used as a producer-side guard) to catch a malformed contract before it ships:
//   - Confidence.Level is one of high|medium|low.
//   - Stale.Coverage is one of the known tiers.
//   - every present EvidenceSpan has a path and a positive line range.
//   - at most MaxNextCalls follow-ups, each with a reason.
func (e Envelope) Validate() error {
	if !validLevel(e.Confidence.Level) {
		return errInvalid("confidence.level", string(e.Confidence.Level))
	}
	if !validCoverage(e.Stale.Coverage) {
		return errInvalid("stale.coverage", string(e.Stale.Coverage))
	}
	for i, ev := range e.Evidence {
		if ev.Path == "" || ev.Start <= 0 || ev.End < ev.Start {
			return errSpan(i, ev)
		}
	}
	if len(e.NextCalls) > MaxNextCalls {
		return errTooMany(len(e.NextCalls))
	}
	for i, nc := range e.NextCalls {
		if nc.Tool == "" || nc.Reason == "" {
			return errNextCall(i)
		}
	}
	return nil
}

func validLevel(l Level) bool {
	switch l {
	case LevelHigh, LevelMedium, LevelLow:
		return true
	}
	return false
}

func validCoverage(c Coverage) bool {
	switch c {
	case CoverageUnknown, CoverageAgeOnly, CoverageCommitOnly, CoverageDirtyWorktree, CoverageFull:
		return true
	}
	return false
}

func errInvalid(field, got string) error {
	return fmt.Errorf("envelope: invalid %s %q", field, got)
}

func errSpan(i int, ev EvidenceSpan) error {
	return fmt.Errorf("envelope: evidence[%d] needs path + positive range, got {path:%q start:%d end:%d}", i, ev.Path, ev.Start, ev.End)
}

func errTooMany(n int) error {
	return fmt.Errorf("envelope: %d next_calls exceeds cap of %d", n, MaxNextCalls)
}

func errNextCall(i int) error {
	return fmt.Errorf("envelope: next_calls[%d] needs both tool and reason", i)
}
