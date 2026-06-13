package benchcompress

import (
	"sort"
	"strings"

	"github.com/alehatsman/dex/internal/compress"
	"github.com/alehatsman/dex/internal/tokens"
)

// PassResult holds metrics for one compress pass over one sample.
type PassResult struct {
	Pass            string  `json:"pass"`
	TokensIn        int     `json:"tokens_in"`
	TokensOut       int     `json:"tokens_out"`
	Ratio           float64 `json:"ratio"`                // tokens_out / tokens_in; lower is better compression
	AnchorPct       float64 `json:"anchor_pct"`           // fraction of anchor tokens preserved verbatim
	ExtractFidelity float64 `json:"extract_fidelity"`     // fraction of answer spans surviving as substrings
	RoundTrip       *bool   `json:"round_trip,omitempty"` // nil for lossy passes
}

// SampleResult aggregates all pass results for one corpus sample.
type SampleResult struct {
	Sample string       `json:"sample"`
	Kind   string       `json:"kind"`
	Passes []PassResult `json:"passes"`
}

// passKind classifies a pass as lossless (true) or lossy (false).
func passKind(pass string) bool {
	switch pass {
	case "codebook", "ngram_codebook", "symmap":
		return true
	}
	return false
}

// RunSample applies every configured pass to s and returns per-pass metrics.
func RunSample(s Sample, family tokens.Family) SampleResult {
	counter := tokens.NewFor(family)
	passes := []string{"aggressive", "entropy", "terse", "ib", "codebook", "ngram_codebook", "symmap"}
	result := SampleResult{Sample: s.Name, Kind: s.Kind}
	for _, p := range passes {
		out := applyPass(p, s.Content)
		pr := measure(p, s, s.Content, out, counter)
		result.Passes = append(result.Passes, pr)
	}
	return result
}

// applyPass runs the named pass and returns the compressed text.
func applyPass(pass, content string) string {
	switch pass {
	case "aggressive":
		return compress.AggressiveCompress(content, ".go")
	case "entropy":
		lines := strings.Split(content, "\n")
		filtered := compress.EntropyFilter(lines, compress.EntropyThresholdStandard)
		return strings.Join(filtered, "\n")
	case "terse":
		res := compress.TerseCompress(content, compress.Level2)
		return res.Output
	case "ib":
		return compress.CompressIB(content, 0.7)
	case "codebook":
		cb := compress.BuildCodebook([]string{content})
		if cb.Empty() {
			return content
		}
		return cb.Legend() + "\n" + cb.Apply(content)
	case "ngram_codebook":
		ncb := compress.BuildNgramCodebook(content)
		return ncb.ApplyWithLegend(content)
	case "symmap":
		sm := compress.BuildSymbolMap(content)
		return sm.ApplyWithLegend(content)
	}
	return content
}

// measure computes metrics for one (pass, sample, compressed) triple.
func measure(pass string, s Sample, original, compressed string, counter tokens.Counter) PassResult {
	tokIn := counter.Count(original)
	tokOut := counter.Count(compressed)
	ratio := 1.0
	if tokIn > 0 {
		ratio = float64(tokOut) / float64(tokIn)
	}

	anchorPct := anchorPreservation(compressed, s.Anchors)
	extractFid := extractiveFidelity(compressed, s.Spans)

	pr := PassResult{
		Pass:            pass,
		TokensIn:        tokIn,
		TokensOut:       tokOut,
		Ratio:           ratio,
		AnchorPct:       anchorPct,
		ExtractFidelity: extractFid,
	}

	if passKind(pass) {
		rt := roundTripCheck(pass, original, compressed)
		pr.RoundTrip = &rt
	}
	return pr
}

// anchorPreservation returns the fraction of anchor tokens that appear verbatim
// in the compressed output.
func anchorPreservation(compressed string, anchors []string) float64 {
	if len(anchors) == 0 {
		return 1.0
	}
	var survived int
	for _, a := range anchors {
		if strings.Contains(compressed, a) {
			survived++
		}
	}
	return float64(survived) / float64(len(anchors))
}

// extractiveFidelity returns the fraction of answer spans that appear as
// substrings in the compressed output.
func extractiveFidelity(compressed string, spans []string) float64 {
	if len(spans) == 0 {
		return 1.0
	}
	var survived int
	for _, sp := range spans {
		if strings.Contains(compressed, sp) {
			survived++
		}
	}
	return float64(survived) / float64(len(spans))
}

// roundTripCheck verifies that the compressed form of a lossless-ish pass
// contains enough information to reconstruct the original. Parses the
// §MAP / ©MAP legend block and applies reverse substitutions.
func roundTripCheck(_ string, original, compressed string) bool {
	// All three passes use the same legend structure; the only difference is the
	// marker prefix used in the body (§N for codebook/symmap, ©N for ngram).
	rev, body := parseLegend(compressed)
	if rev == nil {
		// No legend — pass produced no substitutions; output should equal input.
		return strings.TrimSpace(original) == strings.TrimSpace(compressed)
	}
	reconstructed := applyReverseMap(body, rev)
	return strings.TrimSpace(reconstructed) == strings.TrimSpace(original)
}

// parseLegend splits a compressed string produced by ApplyWithLegend into the
// reverse-substitution map and the body.  Legend format (both § and © variants):
//
//	§MAP:            (or ©MAP:)
//	  §0=value
//	  §1=other value
//	              ← blank line
//	body text
//
// Returns (nil, compressed) when no legend is present.
func parseLegend(compressed string) (rev map[string]string, body string) {
	lines := strings.Split(compressed, "\n")
	if len(lines) == 0 {
		return nil, compressed
	}
	// First line must be the map header.
	header := strings.TrimSpace(lines[0])
	if header != "§MAP:" && header != "©MAP:" {
		return nil, compressed
	}
	rev = make(map[string]string)
	bodyStart := 1
	for i := 1; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" {
			bodyStart = i + 1
			break
		}
		// Each legend line: "  §N=value" or "  ©N=value" or "  sN=value"
		idx := strings.Index(trimmed, "=")
		if idx < 1 {
			continue
		}
		key := trimmed[:idx]
		val := trimmed[idx+1:]
		rev[key] = val
		bodyStart = i + 1
	}
	if len(rev) == 0 {
		return nil, compressed
	}
	body = strings.Join(lines[bodyStart:], "\n")
	return rev, body
}

// applyReverseMap replaces each key in rev with its value across body.
//
// Map iteration order is random, and keys share a prefix (§1 is a prefix of
// §10). Replacing §1 first would corrupt §10 → "<val-of-1>0". Substitute
// longest key first so no key can match inside another (#448).
func applyReverseMap(body string, rev map[string]string) string {
	keys := make([]string, 0, len(rev))
	for k := range rev {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	for _, k := range keys {
		body = strings.ReplaceAll(body, k, rev[k])
	}
	return body
}
