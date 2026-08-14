package retrieve

// ─── per-intent evidence policy (#95d) ─────────────────────────────────────
//
// EvidencePolicy is the single source of truth for what evidence each intent
// pulls: which graph lane anchors the expansion, the inline byte/line budget,
// the symbol-body fill strategy, how many suggested reads, and the answer
// synthesis budget. Before #95d this mapping was smeared across four functions
// (graph.go:runForIntent, inline.go:InlineCapsFor + body-fill, results.go:
// PickSuggestedReads, answer.go:answerMaxTokensFor), each re-deriving the same
// intent buckets in its own switch. The table is data; the lane/fill
// *implementations* stay where they are — only the intent→evidence decision
// moved here. See docs/design/95d-evidence-policy.md.

// GraphLane names the graph-expansion mix an intent anchors on. The lane
// implementation lives in graph.go (runForIntent dispatches on this); the
// table only maps intent → lane.
type GraphLane int

const (
	// GraphLaneNeighborhoodRollup is the default: symbol neighborhood plus a
	// package rollup over the semantic-hit packages. Used by assemble and any
	// unrecognized intent.
	GraphLaneNeighborhoodRollup GraphLane = iota
	// GraphLaneNeighborhood is the symbol neighborhood only — no package
	// rollup. behavior_search omits the rollup deliberately: noisy semHits
	// (e.g. a help-text blob) would otherwise dump a whole package as graph
	// noise (graph.go).
	GraphLaneNeighborhood
	GraphLaneCallersInbound
	GraphLaneCalleesOutbound
	GraphLaneArchitecture
	GraphLanePackageTopology
)

// BodyFill names the symbol-body inline strategy applied during InlineContent.
type BodyFill int

const (
	// BodyFillNone inlines no symbol bodies (reads + semantic hits only).
	BodyFillNone BodyFill = iota
	// BodyFillSymbols inlines symbol bodies in natural order (symbol_lookup).
	BodyFillSymbols
	// BodyFillCoverage inlines symbol bodies in submodular keyword-coverage
	// order (assemble, #687).
	BodyFillCoverage
)

// EvidencePolicy is the per-intent evidence budget. One row per intent lives in
// evidencePolicies; PolicyFor resolves an intent (with a default fallback) to
// its row.
type EvidencePolicy struct {
	GraphLane       GraphLane
	InlineCaps      InlineCaps
	BodyFill        BodyFill
	MaxReads        int
	AnswerMaxTokens int
}

// Inline caps tiers (verbatim from the pre-#95d InlineCapsFor). Dense =
// exploration bundle; targeted = tight bundle. See InlineCaps for the fields.
var (
	capsDense    = InlineCaps{MaxLinesPerRead: 120, MaxBytesPerRead: 8 * 1024, TotalBytesCap: 40 * 1024}
	capsTargeted = InlineCaps{MaxLinesPerRead: 60, MaxBytesPerRead: 4 * 1024, TotalBytesCap: 20 * 1024}

	// capsAssembleDense is a right-sized dense pool for the assemble intent
	// (#164). assemble is the only intent that pairs a dense pool with
	// BodyFillCoverage — submodular body-packing that fills the pool with many
	// symbol bodies — so at capsDense (40 KB) its envelope ceiling lands at
	// 40+10 = 50 KB, exactly the client tool-result reject point (a real
	// (*Server).recallFacts assemble hard-errored at 50,328 chars). 28 KB keeps
	// the empirically-good working set (~28 KB / 14 bodies) while dropping the
	// derived ceiling to 38 KB, leaving headroom for the callers/callees graph.
	capsAssembleDense = InlineCaps{MaxLinesPerRead: 120, MaxBytesPerRead: 8 * 1024, TotalBytesCap: 28 * 1024}
)

// defaultPolicy is returned for auto/unknown intents. It matches the old
// default branches: neighborhood+rollup graph, targeted caps, no body fill,
// 2 reads, 400 answer tokens.
var defaultPolicy = EvidencePolicy{
	GraphLane:       GraphLaneNeighborhoodRollup,
	InlineCaps:      capsTargeted,
	BodyFill:        BodyFillNone,
	MaxReads:        2,
	AnswerMaxTokens: 400,
}

// evidencePolicies maps each concrete intent to its evidence budget. Every cell
// is validated behavior-neutral against the old switches in
// docs/design/95d-evidence-policy.md.
var evidencePolicies = map[string]EvidencePolicy{
	IntentSymbolLookup: {
		GraphLane: GraphLaneNeighborhood, InlineCaps: capsTargeted,
		BodyFill: BodyFillSymbols, MaxReads: 2, AnswerMaxTokens: 400,
	},
	IntentEditingContext: {
		GraphLane: GraphLaneNeighborhood, InlineCaps: capsTargeted,
		BodyFill: BodyFillNone, MaxReads: 2, AnswerMaxTokens: 400,
	},
	IntentBehaviorSearch: {
		GraphLane: GraphLaneNeighborhood, InlineCaps: capsTargeted,
		BodyFill: BodyFillNone, MaxReads: 2, AnswerMaxTokens: 400,
	},
	IntentCallers: {
		GraphLane: GraphLaneCallersInbound, InlineCaps: capsTargeted,
		BodyFill: BodyFillNone, MaxReads: 2, AnswerMaxTokens: 400,
	},
	IntentCallees: {
		GraphLane: GraphLaneCalleesOutbound, InlineCaps: capsTargeted,
		BodyFill: BodyFillNone, MaxReads: 2, AnswerMaxTokens: 400,
	},
	IntentArchitecture: {
		GraphLane: GraphLaneArchitecture, InlineCaps: capsDense,
		BodyFill: BodyFillNone, MaxReads: 5, AnswerMaxTokens: 900,
	},
	IntentPackageTopology: {
		GraphLane: GraphLanePackageTopology, InlineCaps: capsDense,
		BodyFill: BodyFillNone, MaxReads: 5, AnswerMaxTokens: 900,
	},
	// assemble (#687) reuses the default lanes (neighborhood+rollup) but takes
	// a right-sized dense budget (capsAssembleDense, #164 — capsDense overflows
	// the client tool-result limit once coverage body-fill packs it) and
	// coverage-ordered body fill.
	IntentAssemble: {
		GraphLane: GraphLaneNeighborhoodRollup, InlineCaps: capsAssembleDense,
		BodyFill: BodyFillCoverage, MaxReads: 2, AnswerMaxTokens: 400,
	},
}

// PolicyFor returns the evidence policy for an intent, falling back to
// defaultPolicy for auto/unknown intents.
func PolicyFor(intent string) EvidencePolicy {
	if p, ok := evidencePolicies[intent]; ok {
		return p
	}
	return defaultPolicy
}
