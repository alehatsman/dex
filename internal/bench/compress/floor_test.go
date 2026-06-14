package compress

import (
	"strings"
	"testing"

	"github.com/alehatsman/dex/internal/tokens"
)

func ptrBool(b bool) *bool { return &b }

// TestAbsoluteViolations_RealCorpusPasses asserts the live corpus satisfies
// every absolute floor — the gate must be green out of the box.
func TestAbsoluteViolations_RealCorpusPasses(t *testing.T) {
	family := tokens.Detect("")
	var srs []SampleResult
	for _, s := range BuiltinCorpus {
		srs = append(srs, RunSample(s, family))
	}
	rep := Aggregate(srs, family.String())
	if v := rep.AbsoluteViolations(); len(v) > 0 {
		t.Fatalf("real corpus must satisfy absolute floors, got violations:\n  %s",
			strings.Join(v, "\n  "))
	}
	// Sanity: the corpus must include the large samples that make the floors
	// meaningful (dictionary passes only trigger at volume).
	var sawLarge bool
	for _, s := range rep.Samples {
		for _, p := range s.Passes {
			if p.TokensIn >= floorNonTrivialTokens {
				sawLarge = true
			}
		}
	}
	if !sawLarge {
		t.Fatalf("corpus has no non-trivial samples (>=%d tokens) — floors are vacuous", floorNonTrivialTokens)
	}
}

func TestAbsoluteViolations_EmptyNonTrivialInput(t *testing.T) {
	rep := Report{Samples: []SampleResult{{
		Sample: "big-log", Kind: "log",
		Passes: []PassResult{{Pass: "aggressive", TokensIn: 1000, TokensOut: 0}},
	}}}
	if !hasViolation(rep, "emptied a non-trivial input") {
		t.Fatal("expected an empty-output violation")
	}
	// A declined pass on the same input is a no-op, not a violation.
	rep.Samples[0].Passes[0].Declined = true
	rep.Samples[0].Passes[0].TokensOut = 1000
	if hasViolation(rep, "emptied a non-trivial input") {
		t.Fatal("a declined pass must not count as emptying")
	}
}

func TestAbsoluteViolations_LosslessRoundTrip(t *testing.T) {
	rep := Report{Samples: []SampleResult{{
		Sample: "src", Kind: "code",
		Passes: []PassResult{{Pass: "ngram_codebook", TokensIn: 1000, TokensOut: 900, RoundTrip: ptrBool(false)}},
	}}}
	if !hasViolation(rep, "failed round-trip") {
		t.Fatal("expected a round-trip violation")
	}
}

func TestAbsoluteViolations_LossyFidelityFloor(t *testing.T) {
	rep := Report{Summary: []PassSummary{
		{Pass: "aggressive", MeanRatio: 0.4, MeanAnchorPct: 0.2, MeanExtractFid: 0.1}, // lossy: RoundTripOK nil
	}}
	if !hasViolation(rep, "mean anchor") {
		t.Fatal("expected an anchor-floor violation")
	}
	if !hasViolation(rep, "mean extract fidelity") {
		t.Fatal("expected an extract-fidelity-floor violation")
	}
	// A lossless pass with low metrics is gated by round-trip, not fidelity.
	rep = Report{Summary: []PassSummary{
		{Pass: "symmap", MeanAnchorPct: 0.2, MeanExtractFid: 0.1, RoundTripOK: ptrBool(true)},
	}}
	if hasViolation(rep, "mean anchor") {
		t.Fatal("lossless passes must not be subject to the fidelity floor")
	}
}

func TestAbsoluteViolations_DictNeverTriggers(t *testing.T) {
	rep := Report{Samples: []SampleResult{{
		Sample: "big", Kind: "code",
		Passes: []PassResult{
			{Pass: "codebook", TokensIn: 1000, TokensOut: 1000, Ratio: 1.0, RoundTrip: ptrBool(true)},
			{Pass: "ngram_codebook", TokensIn: 1000, TokensOut: 1000, Ratio: 1.0, RoundTrip: ptrBool(true)},
			{Pass: "symmap", TokensIn: 1000, TokensOut: 1000, Ratio: 1.0, RoundTrip: ptrBool(true)},
		},
	}}}
	if !hasViolation(rep, "dictionary passes never triggered") {
		t.Fatal("expected a dictionary-trigger violation when all dict passes sit at ratio 1.0")
	}
	// One dict pass that compresses clears the floor.
	rep.Samples[0].Passes[1].Ratio = 0.7
	if hasViolation(rep, "dictionary passes never triggered") {
		t.Fatal("a single triggering dict pass must clear the floor")
	}
}

func hasViolation(r Report, substr string) bool {
	for _, v := range r.AbsoluteViolations() {
		if strings.Contains(v, substr) {
			return true
		}
	}
	return false
}
