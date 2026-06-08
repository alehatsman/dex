// Package compress provides deterministic text compression passes for
// reducing token usage in LLM contexts without semantic loss.
package compress

import (
	"strings"

	"github.com/alehatsman/dex/internal/tokens"
)

// CompressionLevel controls which terse passes run.
type CompressionLevel int

const (
	Level1 CompressionLevel = 1 // function-word stripping on long lines
	Level2 CompressionLevel = 2 // + abbreviation substitution
	Level3 CompressionLevel = 3 // + zero-unique-token line dedup
)

// TerseResult is the output of TerseCompress.
type TerseResult struct {
	Output         string
	OriginalTokens int
	OutputTokens   int
	// Applied is true when the quality gate passed and compressed output is used.
	Applied bool
}

// minQualitySavings is the minimum token-reduction ratio required for the
// terse pass to be applied (3%). Below this the original is returned unchanged.
const minQualitySavings = 0.03

// maxInputBytes skips terse for very large inputs where per-line processing
// would be expensive and structural compression already handles most of the gain.
const maxInputBytes = 64 * 1024

// longLineTokens is the minimum token count a line must have for Level 1
// function-word stripping to activate (shorter lines rarely benefit).
const longLineTokens = 20

// functionWords is the 50-word blocklist of low-information tokens removed
// from long lines in Level 1. Only whole-word matches are removed.
var functionWords = map[string]bool{
	"a": true, "an": true, "the": true,
	"is": true, "are": true, "was": true, "were": true,
	"be": true, "been": true, "being": true,
	"have": true, "has": true, "had": true,
	"do": true, "does": true, "did": true,
	"will": true, "would": true, "could": true, "should": true,
	"may": true, "might": true, "shall": true, "can": true,
	"to": true, "of": true, "in": true, "on": true, "at": true,
	"by": true, "for": true, "with": true, "from": true, "as": true,
	"or": true, "and": true, "but": true, "not": true,
	"it": true, "its": true, "this": true, "that": true,
	"these": true, "those": true, "which": true, "who": true,
	"whom": true, "what": true, "so": true, "if": true,
}

// abbreviations maps verbose words to compact equivalents for Level 2.
var abbreviations = map[string]string{
	"configuration": "cfg",
	"directory":     "dir",
	"maximum":       "max",
	"minimum":       "min",
	"function":      "fn",
	"parameter":     "param",
	"parameters":    "params",
	"argument":      "arg",
	"arguments":     "args",
	"message":       "msg",
	"messages":      "msgs",
	"response":      "resp",
	"responses":     "resps",
	"request":       "req",
	"requests":      "reqs",
	"interface":     "iface",
	"interfaces":    "ifaces",
	"implementation": "impl",
	"implementations": "impls",
	"initialize":    "init",
	"information":   "info",
	"package":       "pkg",
	"packages":      "pkgs",
	"number":        "num",
	"numbers":       "nums",
	"string":        "str",
	"strings":       "strs",
	"boolean":       "bool",
	"database":      "db",
	"databases":     "dbs",
	"environment":   "env",
	"environments":  "envs",
	"variable":      "var",
	"variables":     "vars",
	"temporary":     "tmp",
	"address":       "addr",
	"addresses":     "addrs",
	"connection":    "conn",
	"connections":   "conns",
	"timeout":       "tmo",
	"timestamp":     "ts",
	"pointer":       "ptr",
	"buffer":        "buf",
	"buffers":       "bufs",
	"length":        "len",
	"index":         "idx",
	"iterator":      "iter",
	"output":        "out",
	"input":         "in",
}

// TerseCompress applies up to `level` deterministic compression passes to
// input and returns the result. The quality gate (3% token savings minimum)
// must pass for compressed output to be used; otherwise Applied=false and
// Output equals input.
//
// Inputs larger than 64 KB are returned unchanged (Applied=false).
func TerseCompress(input string, level CompressionLevel) TerseResult {
	if len(input) > maxInputBytes || strings.TrimSpace(input) == "" {
		toks := countTokens(input)
		return TerseResult{Output: input, OriginalTokens: toks, OutputTokens: toks}
	}

	origTokens := countTokens(input)
	lines := strings.Split(input, "\n")

	if level >= Level1 {
		lines = stripFunctionWords(lines)
	}
	if level >= Level2 {
		lines = applyAbbreviations(lines)
	}
	if level >= Level3 {
		lines = dedupZeroUnique(lines)
	}

	out := strings.Join(lines, "\n")
	outTokens := countTokens(out)

	saved := origTokens - outTokens
	if origTokens == 0 || float64(saved)/float64(origTokens) < minQualitySavings {
		return TerseResult{Output: input, OriginalTokens: origTokens, OutputTokens: origTokens}
	}
	return TerseResult{
		Output:         out,
		OriginalTokens: origTokens,
		OutputTokens:   outTokens,
		Applied:        true,
	}
}

// stripFunctionWords removes function words from lines that have more than
// longLineTokens tokens. Short lines are left intact — removing "a" from a
// 3-word line risks changing meaning.
func stripFunctionWords(lines []string) []string {
	out := make([]string, len(lines))
	for i, line := range lines {
		words := strings.Fields(line)
		if len(words) <= longLineTokens {
			out[i] = line
			continue
		}
		filtered := words[:0]
		for _, w := range words {
			if !functionWords[strings.ToLower(w)] {
				filtered = append(filtered, w)
			}
		}
		out[i] = strings.Join(filtered, " ")
	}
	return out
}

// applyAbbreviations replaces whole-word occurrences of verbose terms with
// their compact equivalents (case-insensitive match, preserves surrounding
// punctuation as long as the word boundary holds).
func applyAbbreviations(lines []string) []string {
	out := make([]string, len(lines))
	for i, line := range lines {
		words := strings.Fields(line)
		for j, w := range words {
			// Strip trailing punctuation for lookup, reattach after.
			stripped := strings.TrimRight(w, ".,;:!?\"'")
			suffix := w[len(stripped):]
			if abbr, ok := abbreviations[strings.ToLower(stripped)]; ok {
				// Preserve leading capitalisation if the original was capitalised.
				if len(stripped) > 0 && stripped[0] >= 'A' && stripped[0] <= 'Z' {
					abbr = strings.ToUpper(abbr[:1]) + abbr[1:]
				}
				words[j] = abbr + suffix
			}
		}
		out[i] = strings.Join(words, " ")
	}
	return out
}

// dedupZeroUnique drops lines whose every token already appeared in the
// previous 3 lines (sliding window). Empty lines are never dropped.
func dedupZeroUnique(lines []string) []string {
	const windowSize = 3
	window := make([]map[string]struct{}, 0, windowSize)

	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			out = append(out, line)
			continue
		}
		toks := tokenSet(line)
		if len(toks) == 0 {
			out = append(out, line)
			continue
		}

		// Build union of tokens seen in the window.
		seen := make(map[string]struct{})
		for _, wm := range window {
			for t := range wm {
				seen[t] = struct{}{}
			}
		}

		// Count tokens unique to this line (not in window).
		uniqueCount := 0
		for t := range toks {
			if _, inSeen := seen[t]; !inSeen {
				uniqueCount++
			}
		}

		if uniqueCount == 0 {
			// All tokens already seen — drop the line.
			continue
		}

		out = append(out, line)
		window = append(window, toks)
		if len(window) > windowSize {
			window = window[1:]
		}
	}
	return out
}

// tokenSet returns the set of lowercase word tokens in a line.
func tokenSet(line string) map[string]struct{} {
	words := strings.Fields(line)
	out := make(map[string]struct{}, len(words))
	for _, w := range words {
		out[strings.ToLower(w)] = struct{}{}
	}
	return out
}

// AbbreviateText applies the abbreviation dictionary to a single string,
// replacing whole-word verbose terms with their compact equivalents.
// Trailing punctuation is stripped before lookup and reattached after.
func AbbreviateText(s string) string {
	words := strings.Fields(s)
	for j, w := range words {
		stripped := strings.TrimRight(w, ".,;:!?\"'")
		suffix := w[len(stripped):]
		if abbr, ok := abbreviations[strings.ToLower(stripped)]; ok {
			if len(stripped) > 0 && stripped[0] >= 'A' && stripped[0] <= 'Z' {
				abbr = strings.ToUpper(abbr[:1]) + abbr[1:]
			}
			words[j] = abbr + suffix
		}
	}
	return strings.Join(words, " ")
}

// countTokens returns a real BPE token count (default o200k_base) so the
// terse pass's OriginalTokens/OutputTokens and the adaptive feedback ratio
// reflect what the model actually sees, not a whitespace-word approximation.
func countTokens(s string) int { return tokens.Count(s) }
