package compress

import "strings"

// compressionThresholds holds per-language parameters that control how
// aggressively compression passes treat source files of that language.
type compressionThresholds struct {
	// minEntropyBitsPerChar drives the entropy filter threshold for this
	// language. Higher = more information-dense → keep more lines.
	minEntropyBitsPerChar float64
	// jaccardDedup is the Jaccard similarity threshold above which near-
	// duplicate lines are merged (reserved for future use).
	jaccardDedup float64
	// autoDelta scales blank/boilerplate removal aggressiveness.
	autoDelta float64
}

// entropyFilterThreshold maps minEntropyBitsPerChar (calibrated 0.60–1.20)
// to an EntropyFilter line-score threshold (EntropyThresholdLite–Max).
// Dense languages (Python, Ruby) → higher threshold → keep more lines.
// Verbose/repetitive languages (JSON, Java) → lower threshold → drop more.
func (t compressionThresholds) entropyFilterThreshold() float64 {
	const minBPE, maxBPE = 0.60, 1.20
	frac := (t.minEntropyBitsPerChar - minBPE) / (maxBPE - minBPE)
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	return EntropyThresholdLite + frac*(EntropyThresholdMax-EntropyThresholdLite)
}

var langThresholds = map[string]compressionThresholds{
	"go":   {0.90, 0.72, 0.55},
	"py":   {1.20, 0.65, 0.55},
	"rs":   {0.85, 0.72, 0.60},
	"ts":   {0.95, 0.68, 0.58},
	"tsx":  {0.95, 0.68, 0.58},
	"js":   {1.00, 0.68, 0.58},
	"jsx":  {1.00, 0.68, 0.58},
	"java": {0.80, 0.65, 0.50},
	"json": {0.60, 0.60, 0.40},
	"yaml": {0.70, 0.62, 0.45},
	"yml":  {0.70, 0.62, 0.45},
	"h":    {0.75, 0.65, 0.50},
	"rb":   {1.15, 0.65, 0.55},
}

var defaultThresholds = compressionThresholds{0.85, 0.65, 0.55}

// thresholdsFor returns the compression thresholds for the given file
// extension. ext may include a leading dot or not (e.g. ".go" or "go").
func thresholdsFor(ext string) compressionThresholds {
	e := strings.ToLower(strings.TrimPrefix(ext, "."))
	if t, ok := langThresholds[e]; ok {
		return t
	}
	return defaultThresholds
}

// dropLowEntropyLines filters lines below threshold using per-line entropy
// scoring. Blank lines are always preserved, and declaration-signature lines
// are floored (kept regardless of entropy) so a distinct func/type/class
// signature is never dropped on novelty grounds — mirroring the anchor floor
// dropLowEntropyLinesStrict gets from lineHasAnchor (#456). No quality gate is
// applied — AggressiveCompress uses SafeguardRatio as the safety net.
func dropLowEntropyLines(lines []string, threshold float64) []string {
	lineTg := make([]map[string]struct{}, len(lines))
	for i, line := range lines {
		lineTg[i] = charTrigrams(line)
	}

	seen := make(map[string]struct{}, 256)
	out := make([]string, 0, len(lines))
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			out = append(out, line)
			continue
		}
		keep := isDeclarationLine(line) ||
			lineScore(line, seen, windowTrigrams(lineTg, i)) >= threshold
		if keep {
			out = append(out, line)
			for tg := range lineTg[i] {
				seen[tg] = struct{}{}
			}
		}
	}
	return out
}

// declKeywords are leading keywords marking a distinct declaration whose
// signature must survive the relaxed entropy pass even when it textually
// resembles an earlier kept line. Kept deliberately keyword-based: languages
// whose member declarations carry no keyword (e.g. Java methods) are not
// specially floored here, but their signatures usually carry a PascalCase or
// qualified anchor token that already scores above threshold.
var declKeywords = []string{
	"func ", "func(", // Go (incl. methods: "func (r R) Name")
	"def ", // Python / Ruby
	"fn ",  // Rust
	"class ", "struct ", "enum ", "trait ", "impl ", "interface ",
	"type ", "function ",
}

// declModifiers are visibility/qualifier keywords peeled off the front before
// the keyword check, so "pub fn", "export class", "async def", and
// "export default class" still register as declarations.
var declModifiers = []string{
	"pub ", "export ", "default ", "async ",
	"public ", "private ", "protected ", "abstract ", "final ", "static ",
}

// isDeclarationLine reports whether line begins a declaration signature after
// trimming indentation and peeling leading visibility/qualifier modifiers.
func isDeclarationLine(line string) bool {
	s := strings.TrimSpace(line)
	for {
		peeled := false
		for _, m := range declModifiers {
			if strings.HasPrefix(s, m) {
				s = s[len(m):]
				peeled = true
			}
		}
		if !peeled {
			break
		}
	}
	for _, kw := range declKeywords {
		if strings.HasPrefix(s, kw) {
			return true
		}
	}
	return false
}

// dropLowEntropyLinesStrict is dropLowEntropyLines that never drops a line
// containing an anchor, so anchor tokens survive verbatim under a strict
// target_model regardless of the line's entropy score.
func dropLowEntropyLinesStrict(lines []string, threshold float64, a AnchorSet) []string {
	lineTg := make([]map[string]struct{}, len(lines))
	for i, line := range lines {
		lineTg[i] = charTrigrams(line)
	}

	seen := make(map[string]struct{}, 256)
	out := make([]string, 0, len(lines))
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			out = append(out, line)
			continue
		}
		keep := a.lineHasAnchor(line) ||
			lineScore(line, seen, windowTrigrams(lineTg, i)) >= threshold
		if keep {
			out = append(out, line)
			for tg := range lineTg[i] {
				seen[tg] = struct{}{}
			}
		}
	}
	return out
}
