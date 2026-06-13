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
