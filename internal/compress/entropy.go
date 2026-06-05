package compress

import (
	"math"
	"regexp"
	"strings"
)

// Entropy filter threshold levels — bits/char combined score below which a
// line is dropped. Standard is the safe default for command output.
const (
	EntropyThresholdLite     = 2.5
	EntropyThresholdStandard = 3.0
	EntropyThresholdMax      = 3.5
)

// EntropyFilter drops low-information lines from command output using a
// three-layer pipeline:
//
//  1. Strip pure decoration lines and known filler patterns.
//  2. Per-line entropy scoring: entropy + marker - repetition_penalty.
//     Lines scoring below threshold are dropped.
//  3. Quality gate: if the filtered output drops any file path or >10% of
//     long identifiers from the original, the original is returned instead
//     (nil signals "no improvement").
//
// Returns nil when the quality gate rejects the result or fewer than 3%
// of tokens are saved (not worth the noise).
func EntropyFilter(lines []string, threshold float64) []string {
	if len(lines) < 5 {
		return nil
	}

	// Layer 3: strip decorations and filler first.
	stripped := stripDecorations(lines)
	stripped = stripFiller(stripped)

	// Layer 1: per-line entropy + marker - repetition scoring.
	seenTrigrams := make(map[string]struct{}, 256)
	out := make([]string, 0, len(stripped))
	for _, line := range stripped {
		if strings.TrimSpace(line) == "" {
			out = append(out, line)
			continue
		}
		score := lineScore(line, seenTrigrams)
		if score >= threshold {
			out = append(out, line)
			for tg := range charTrigrams(line) {
				seenTrigrams[tg] = struct{}{}
			}
		}
	}

	// Layer 2: quality gate — check path and identifier preservation.
	if !qualityGate(lines, out) {
		return nil
	}

	// Minimum savings guard: 3% token reduction required.
	origTok := countTokens(strings.Join(lines, "\n"))
	outTok := countTokens(strings.Join(out, "\n"))
	if origTok == 0 || float64(origTok-outTok)/float64(origTok) < 0.03 {
		return nil
	}

	return out
}

// lineScore computes the combined information score for a line:
//
//	score = entropy(line) + marker(line) − 0.5×trigramOverlap(line, seen)
//
// clamped to ≥ 0.
func lineScore(line string, seen map[string]struct{}) float64 {
	ent := shannonEntropy(line)
	mark := markerScore(line)
	tg := charTrigrams(line)
	rep := 0.5 * trigramOverlapRatio(tg, seen)
	score := ent + mark - rep
	if score < 0 {
		return 0
	}
	return score
}

// shannonEntropy computes character-level Shannon entropy (bits per char).
func shannonEntropy(s string) float64 {
	if len(s) == 0 {
		return 0
	}
	var freq [256]int
	for _, c := range []byte(s) {
		freq[c]++
	}
	n := float64(len(s))
	var h float64
	for _, f := range freq {
		if f > 0 {
			p := float64(f) / n
			h -= p * math.Log2(p)
		}
	}
	return h
}

// markerScore returns +0.3 if the line has any high-signal marker:
// a file path (word/word.ext), a digit, an error/warning keyword, or ≥2
// long identifiers (≥6 chars).
func markerScore(line string) float64 {
	lower := strings.ToLower(line)
	if rePathExt.MatchString(line) ||
		containsDigit(line) ||
		containsAny(lower, errorWarningKWs) ||
		countLongIdents(line) >= 2 {
		return 0.3
	}
	return 0
}

var rePathExt = regexp.MustCompile(`\w+/\w[^/\s]*\.\w+`)

var errorWarningKWs = []string{
	"error", "warn", "fail", "fatal", "panic", "exception",
	"critical", "abort", "traceback",
}

func containsDigit(s string) bool {
	for _, c := range s {
		if c >= '0' && c <= '9' {
			return true
		}
	}
	return false
}

// countLongIdents counts word tokens with ≥6 alphabetic characters.
func countLongIdents(line string) int {
	count := 0
	for _, w := range strings.Fields(line) {
		alpha := 0
		for _, c := range w {
			if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
				alpha++
			}
		}
		if alpha >= 6 {
			count++
		}
	}
	return count
}

// charTrigrams returns the set of 3-character substrings in s.
func charTrigrams(s string) map[string]struct{} {
	b := []byte(s)
	if len(b) < 3 {
		return nil
	}
	out := make(map[string]struct{}, len(b)-2)
	for i := 0; i <= len(b)-3; i++ {
		out[string(b[i:i+3])] = struct{}{}
	}
	return out
}

// trigramOverlapRatio returns the fraction of trigrams in tg that are already
// in seen (0.0–1.0).
func trigramOverlapRatio(tg map[string]struct{}, seen map[string]struct{}) float64 {
	if len(tg) == 0 {
		return 0
	}
	overlap := 0
	for t := range tg {
		if _, ok := seen[t]; ok {
			overlap++
		}
	}
	return float64(overlap) / float64(len(tg))
}

// ── decoration and filler ────────────────────────────────────────────────────

var decorChars = map[rune]bool{
	'=': true, '-': true, '*': true,
	'─': true, '━': true, '▀': true, '▄': true,
	'╔': true, '║': true, '░': true, '█': true, '═': true,
	'#': true, '+': true,
}

// isPureDecoration reports whether a line is visual noise with no information
// value: >60% the same decoration character, lone separators, or empty
// comment lines.
func isPureDecoration(line string) bool {
	t := strings.TrimSpace(line)
	if t == "" || t == "|" || t == "| " {
		return true
	}

	runes := []rune(t)
	n := len(runes)
	if n == 0 {
		return false
	}

	// Count decoration characters.
	counts := make(map[rune]int)
	for _, c := range runes {
		if decorChars[c] {
			counts[c]++
		}
	}
	for _, cnt := range counts {
		if float64(cnt)/float64(n) > 0.6 {
			return true
		}
	}

	// Comment lines whose content is empty or all decoration chars.
	prefixes := []string{"//", "#", "--"}
	for _, pfx := range prefixes {
		if strings.HasPrefix(t, pfx) {
			rest := strings.TrimSpace(t[len(pfx):])
			if rest == "" {
				return true
			}
			allDecor := true
			for _, c := range rest {
				if !decorChars[c] && c != ' ' {
					allDecor = false
					break
				}
			}
			if allDecor {
				return true
			}
		}
	}

	return false
}

// stripDecorations removes pure decoration lines.
func stripDecorations(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if !isPureDecoration(l) {
			out = append(out, l)
		}
	}
	return out
}

// fillerPatterns lists known low-signal suffixes and noise strings found in
// common tool output (git, cargo, npm, docker).
var fillerPatterns = []string{
	`use "git add`,
	`use "git restore`,
	`(use "git`,
	"run with RUST_BACKTRACE",
	"for more information about this error",
	"try `rustc --explain",
	"run `npm fund`",
	"run `npm audit`",
	"to address all issues",
	"sending build context",
	"using cache",
	"packages are looking for funding",
	"no changes added to commit",
	"= note: ",
	"---> running in",
}

// stripFiller removes lines that match known low-signal filler patterns.
func stripFiller(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		lower := strings.ToLower(l)
		matched := false
		for _, p := range fillerPatterns {
			if strings.Contains(lower, strings.ToLower(p)) {
				matched = true
				break
			}
		}
		if !matched {
			out = append(out, l)
		}
	}
	return out
}

// ── quality gate ─────────────────────────────────────────────────────────────

var reQualityPath = regexp.MustCompile(`\b\w[\w.-]*/[\w.-]+\.\w+\b`)

// extractPaths returns all file-path-like tokens from text.
func extractPaths(lines []string) []string {
	text := strings.Join(lines, "\n")
	return reQualityPath.FindAllString(text, -1)
}

// extractLongIdents returns all word tokens with ≥6 alphabetic characters.
func extractLongIdents(lines []string) []string {
	text := strings.Join(lines, "\n")
	var out []string
	for _, w := range strings.Fields(text) {
		alpha := 0
		for _, c := range w {
			if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
				alpha++
			}
		}
		if alpha >= 6 {
			out = append(out, w)
		}
	}
	return out
}

// qualityGate returns true when the compressed output preserves:
//   - 100% of file paths from the original
//   - ≥90% of long identifiers (≥6 chars) from the original
func qualityGate(original, compressed []string) bool {
	outText := strings.Join(compressed, "\n")

	// Path preservation: every path in original must appear in output.
	for _, path := range extractPaths(original) {
		if !strings.Contains(outText, path) {
			return false
		}
	}

	// Identifier preservation: ≥90% of long idents must survive.
	origIdents := extractLongIdents(original)
	if len(origIdents) == 0 {
		return true
	}
	preserved := 0
	for _, id := range origIdents {
		if strings.Contains(outText, id) {
			preserved++
		}
	}
	return float64(preserved)/float64(len(origIdents)) >= 0.90
}

