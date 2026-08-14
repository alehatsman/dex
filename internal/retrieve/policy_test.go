package retrieve

import "testing"

// TestPolicyFor pins every intent → its full evidence policy row (the
// intent→lanes contract #104 asks for). If a policy cell changes, this test
// documents the deliberate retune; a silent drift fails here.
func TestPolicyFor(t *testing.T) {
	cases := []struct {
		intent string
		want   EvidencePolicy
	}{
		{IntentSymbolLookup, EvidencePolicy{GraphLaneNeighborhood, capsTargeted, BodyFillSymbols, 2, 400}},
		{IntentEditingContext, EvidencePolicy{GraphLaneNeighborhood, capsTargeted, BodyFillNone, 2, 400}},
		{IntentBehaviorSearch, EvidencePolicy{GraphLaneNeighborhood, capsTargeted, BodyFillNone, 2, 400}},
		{IntentCallers, EvidencePolicy{GraphLaneCallersInbound, capsTargeted, BodyFillNone, 2, 400}},
		{IntentCallees, EvidencePolicy{GraphLaneCalleesOutbound, capsTargeted, BodyFillNone, 2, 400}},
		{IntentArchitecture, EvidencePolicy{GraphLaneArchitecture, capsDense, BodyFillNone, 5, 900}},
		{IntentPackageTopology, EvidencePolicy{GraphLanePackageTopology, capsDense, BodyFillNone, 5, 900}},
		{IntentAssemble, EvidencePolicy{GraphLaneNeighborhoodRollup, capsAssembleDense, BodyFillCoverage, 2, 400}},
		// auto/unknown fall back to defaultPolicy.
		{IntentAuto, defaultPolicy},
		{"nonsense-intent", defaultPolicy},
	}
	for _, tc := range cases {
		if got := PolicyFor(tc.intent); got != tc.want {
			t.Errorf("PolicyFor(%q) = %+v, want %+v", tc.intent, got, tc.want)
		}
	}
}

// TestPolicyForMatchesLegacyHelpers guards behavior-neutrality: the three
// helper functions that used to hold their own intent switches must now agree
// with the table for every known intent.
func TestPolicyForMatchesLegacyHelpers(t *testing.T) {
	intents := []string{
		IntentSymbolLookup, IntentEditingContext, IntentBehaviorSearch,
		IntentCallers, IntentCallees, IntentArchitecture,
		IntentPackageTopology, IntentAssemble, IntentAuto,
	}
	for _, in := range intents {
		p := PolicyFor(in)
		if got := InlineCapsFor(in); got != p.InlineCaps {
			t.Errorf("InlineCapsFor(%q) = %+v, want %+v", in, got, p.InlineCaps)
		}
		if got := answerMaxTokensFor(in); got != p.AnswerMaxTokens {
			t.Errorf("answerMaxTokensFor(%q) = %d, want %d", in, got, p.AnswerMaxTokens)
		}
	}
}

// TestInlineCapsTiers pins the two budget tiers verbatim — these bytes are the
// contract other callers (results.go maxReads reasoning, docs) refer to.
func TestInlineCapsTiers(t *testing.T) {
	if capsDense != (InlineCaps{MaxLinesPerRead: 120, MaxBytesPerRead: 8 * 1024, TotalBytesCap: 40 * 1024}) {
		t.Errorf("capsDense drifted: %+v", capsDense)
	}
	if capsTargeted != (InlineCaps{MaxLinesPerRead: 60, MaxBytesPerRead: 4 * 1024, TotalBytesCap: 20 * 1024}) {
		t.Errorf("capsTargeted drifted: %+v", capsTargeted)
	}
}

// #164: assemble must use a smaller dense pool than the shared capsDense, whose
// 40 KB cap overflowed the client tool-result limit once BodyFillCoverage packed
// it. The assemble policy must point at that smaller pool.
func TestAssemblePoolSmallerThanDense(t *testing.T) {
	if capsAssembleDense.TotalBytesCap >= capsDense.TotalBytesCap {
		t.Errorf("capsAssembleDense=%d must be < capsDense=%d", capsAssembleDense.TotalBytesCap, capsDense.TotalBytesCap)
	}
	if got := PolicyFor(IntentAssemble).InlineCaps; got != capsAssembleDense {
		t.Errorf("assemble policy caps=%+v, want capsAssembleDense=%+v", got, capsAssembleDense)
	}
}
