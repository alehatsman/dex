package compress

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Report aggregates compress-bench results across all samples.
type Report struct {
	Tokenizer string         `json:"tokenizer"`
	Samples   []SampleResult `json:"samples"`
	Summary   []PassSummary  `json:"summary"`
}

// PassSummary is the per-pass aggregate over all samples.
type PassSummary struct {
	Pass           string  `json:"pass"`
	MeanRatio      float64 `json:"mean_ratio"`
	MeanAnchorPct  float64 `json:"mean_anchor_pct"`
	MeanExtractFid float64 `json:"mean_extract_fidelity"`
	RoundTripOK    *bool   `json:"round_trip_ok,omitempty"` // nil for lossy passes
}

// Aggregate builds summary statistics from sample results.
func Aggregate(samples []SampleResult, tokenizer string) Report {
	// collect per-pass accumulators
	type acc struct {
		ratio, anchor, extract float64
		n                      int
		rtOK                   *bool // only set for lossless passes
	}
	m := make(map[string]*acc)

	for _, sr := range samples {
		for _, pr := range sr.Passes {
			a := m[pr.Pass]
			if a == nil {
				a = &acc{}
				m[pr.Pass] = a
			}
			a.ratio += pr.Ratio
			a.anchor += pr.AnchorPct
			a.extract += pr.ExtractFidelity
			a.n++
			if pr.RoundTrip != nil {
				if a.rtOK == nil {
					ok := *pr.RoundTrip
					a.rtOK = &ok
				} else {
					*a.rtOK = *a.rtOK && *pr.RoundTrip
				}
			}
		}
	}

	passOrder := []string{"aggressive", "entropy", "terse", "ib", "codebook", "ngram_codebook", "symmap"}
	summary := make([]PassSummary, 0, len(passOrder))
	for _, p := range passOrder {
		a := m[p]
		if a == nil || a.n == 0 {
			continue
		}
		ps := PassSummary{
			Pass:           p,
			MeanRatio:      a.ratio / float64(a.n),
			MeanAnchorPct:  a.anchor / float64(a.n),
			MeanExtractFid: a.extract / float64(a.n),
			RoundTripOK:    a.rtOK,
		}
		summary = append(summary, ps)
	}

	return Report{
		Tokenizer: tokenizer,
		Samples:   samples,
		Summary:   summary,
	}
}

// JSON serialises the report.
func (r Report) JSON() ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}

// Markdown renders a human-readable summary table.
func (r Report) Markdown() string {
	var sb strings.Builder
	sb.WriteString("## dex bench compress\n\n")
	sb.WriteString(fmt.Sprintf("tokenizer: `%s`  samples: %d\n\n", r.Tokenizer, len(r.Samples)))
	sb.WriteString("| pass | ratio | anchor% | extract% | round-trip |\n")
	sb.WriteString("|------|------:|--------:|---------:|:----------:|\n")
	for _, ps := range r.Summary {
		rt := "—"
		if ps.RoundTripOK != nil {
			if *ps.RoundTripOK {
				rt = "✓"
			} else {
				rt = "✗ FAIL"
			}
		}
		sb.WriteString(fmt.Sprintf("| %-16s | %.3f | %6.1f%% | %7.1f%% | %-10s |\n",
			ps.Pass,
			ps.MeanRatio,
			ps.MeanAnchorPct*100,
			ps.MeanExtractFid*100,
			rt,
		))
	}
	sb.WriteByte('\n')
	return sb.String()
}

// Regressions checks the current report against a baseline and returns a
// slice of human-readable regression descriptions. The tolerance parameter
// applies to ratio (higher is worse), anchor%, and extractive fidelity (lower
// is worse). Round-trip failures are always regressions.
func (r Report) Regressions(baseline Report, tol float64) []string {
	base := make(map[string]PassSummary, len(baseline.Summary))
	for _, ps := range baseline.Summary {
		base[ps.Pass] = ps
	}

	var out []string
	for _, cur := range r.Summary {
		b, ok := base[cur.Pass]
		if !ok {
			continue
		}
		// Ratio regression: current is significantly higher than baseline (worse compression).
		if cur.MeanRatio-b.MeanRatio > tol {
			out = append(out, fmt.Sprintf("pass %s: ratio regressed %.3f→%.3f (+%.3f > tol %.2f)",
				cur.Pass, b.MeanRatio, cur.MeanRatio, cur.MeanRatio-b.MeanRatio, tol))
		}
		// Anchor regression: dropped too much.
		if b.MeanAnchorPct-cur.MeanAnchorPct > tol {
			out = append(out, fmt.Sprintf("pass %s: anchor_pct regressed %.3f→%.3f (delta %.3f > tol %.2f)",
				cur.Pass, b.MeanAnchorPct, cur.MeanAnchorPct, b.MeanAnchorPct-cur.MeanAnchorPct, tol))
		}
		// Extractive fidelity regression.
		if b.MeanExtractFid-cur.MeanExtractFid > tol {
			out = append(out, fmt.Sprintf("pass %s: extract_fidelity regressed %.3f→%.3f (delta %.3f > tol %.2f)",
				cur.Pass, b.MeanExtractFid, cur.MeanExtractFid, b.MeanExtractFid-cur.MeanExtractFid, tol))
		}
		// Round-trip hard failure.
		if cur.RoundTripOK != nil && !*cur.RoundTripOK {
			out = append(out, fmt.Sprintf("pass %s: round-trip reconstruction FAILED", cur.Pass))
		}
	}
	return out
}

// Absolute floor thresholds — baseline-independent invariants enforced in
// addition to the per-metric regression check. A pass can satisfy the baseline
// and still be broken: emit empty output, destroy every answer span, or never
// trigger at all (the original ratio-1.000 dictionary passes). These floors
// fail the gate regardless of what the baseline blessed (#492).
const (
	// floorNonTrivialTokens marks an input large enough that emptying it, or
	// shedding most of its content, is a defect rather than metric noise on a
	// tiny snippet (where the entropy surprise metric legitimately degenerates).
	floorNonTrivialTokens = 400
	// floorAnchorPct / floorExtractFidelity: across the corpus a lossy pass must
	// preserve at least half its anchor tokens and answer spans.
	floorAnchorPct       = 0.5
	floorExtractFidelity = 0.5
	// floorDictTriggerRatio: the lossless dictionary passes must demonstrably pay
	// off at volume — at least one non-trivial sample must compress to <= this.
	// Guards the original finding that they sat ungated at ratio 1.000.
	floorDictTriggerRatio = 0.90
)

// dictPasses are the lossless dictionary passes whose entire value is folding
// repeated tokens; on a representative corpus at least one must actually fire.
var dictPasses = map[string]bool{"codebook": true, "ngram_codebook": true, "symmap": true}

// AbsoluteViolations returns baseline-independent floor violations. An empty
// slice means the corpus satisfies every absolute invariant. This runs even
// when no baseline exists, so a degenerate pass can never "pass forever" merely
// because the baseline already recorded its broken numbers.
func (r Report) AbsoluteViolations() []string {
	var v []string

	// 1. No pass may empty a non-trivial input. A pass that explicitly declined
	//    is recorded as a no-op (TokensOut == TokensIn), so a genuine 0 here is a
	//    real emptying, not the EntropyFilter decline sentinel.
	for _, s := range r.Samples {
		for _, p := range s.Passes {
			if p.TokensIn >= floorNonTrivialTokens && !p.Declined && p.TokensOut == 0 {
				v = append(v, fmt.Sprintf("%s/%s: emptied a non-trivial input (%d tokens → 0)",
					s.Sample, p.Pass, p.TokensIn))
			}
		}
	}

	// 2. Every lossless pass must round-trip on every sample (RoundTrip is non-nil
	//    only for lossless passes).
	for _, s := range r.Samples {
		for _, p := range s.Passes {
			if p.RoundTrip != nil && !*p.RoundTrip {
				v = append(v, fmt.Sprintf("%s/%s: lossless pass failed round-trip", s.Sample, p.Pass))
			}
		}
	}

	// 3. Aggregate lossy fidelity floor. Lossless passes carry RoundTripOK and are
	//    gated by rule 2 instead; lossy passes (RoundTripOK == nil) must keep at
	//    least half their anchors and answer spans across the corpus.
	for _, ps := range r.Summary {
		if ps.RoundTripOK != nil {
			continue
		}
		if ps.MeanAnchorPct < floorAnchorPct {
			v = append(v, fmt.Sprintf("pass %s: mean anchor %.2f < floor %.2f", ps.Pass, ps.MeanAnchorPct, floorAnchorPct))
		}
		if ps.MeanExtractFid < floorExtractFidelity {
			v = append(v, fmt.Sprintf("pass %s: mean extract fidelity %.2f < floor %.2f", ps.Pass, ps.MeanExtractFid, floorExtractFidelity))
		}
	}

	// 4. Dictionary passes must trigger at volume: across non-trivial samples, the
	//    best (lowest) ratio of any dictionary pass must beat the trigger floor.
	bestDict, sawNonTrivial := 2.0, false
	for _, s := range r.Samples {
		for _, p := range s.Passes {
			if p.TokensIn < floorNonTrivialTokens || !dictPasses[p.Pass] {
				continue
			}
			sawNonTrivial = true
			if p.Ratio < bestDict {
				bestDict = p.Ratio
			}
		}
	}
	if sawNonTrivial && bestDict > floorDictTriggerRatio {
		v = append(v, fmt.Sprintf("dictionary passes never triggered: best ratio %.3f > floor %.2f (corpus has no compressible volume)",
			bestDict, floorDictTriggerRatio))
	}

	return v
}

// CheckRegression loads a baseline JSON file and fails if any metric regressed.
func CheckRegression(current Report, baselinePath string) error {
	data, err := os.ReadFile(baselinePath)
	if err != nil {
		return fmt.Errorf("read baseline %q: %w", baselinePath, err)
	}
	var baseline Report
	if err := json.Unmarshal(data, &baseline); err != nil {
		return fmt.Errorf("parse baseline: %w", err)
	}
	const tol = 0.05
	regs := current.Regressions(baseline, tol)
	if len(regs) == 0 {
		return nil
	}
	return fmt.Errorf("%s (tol %.2f)", strings.Join(regs, "; "), tol)
}
