package mcp

import (
	"strings"
)

// chunkImportanceScore scores a code excerpt by structural landmark density
// for attention-model layout (L-curve: highest-signal content first).
//
// Weights mirror the issue spec:
//
//	error/panic/fatal  2.0
//	import/require     1.6
//	func/class/type    1.5
//	comment            1.2
//	return             1.0
//	if/for             0.9
//	assert             0.8
//	lone closing brace 0.3
//
// Score is the average weight across all lines, so longer chunks don't
// automatically win just by having more lines.
func chunkImportanceScore(content string) float64 {
	lines := strings.Split(content, "\n")
	if len(lines) == 0 {
		return 0
	}
	var total float64
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		total += lineWeight(line)
	}
	return total / float64(len(lines))
}

func lineWeight(line string) float64 {
	if line == "" {
		return 0
	}
	lower := strings.ToLower(line)

	switch {
	case attentionContainsAny(lower, "error", "panic", "fatal", "exception", "stderr"):
		return 2.0
	case startsWithAny(lower, "import ", "from ", "require(", "#include", "use "):
		return 1.6
	case startsWithAny(lower, "func ", "function ", "def ", "class ", "type ", "struct ", "interface ", "enum ", "pub fn ", "fn "):
		return 1.5
	case len(line) > 1 && (line[:2] == "//" || line[:1] == "#" || line[:1] == "*" || (len(line) > 2 && line[:3] == "/**")):
		return 1.2
	case startsWithAny(lower, "return ", "return\t", "return\n", "yield "):
		return 1.0
	case startsWithAny(lower, "if ", "for ", "while ", "switch ", "match "):
		return 0.9
	case startsWithAny(lower, "assert", "expect(", "test(", "it(", "describe("):
		return 0.8
	case line == "}" || line == ")" || line == "]":
		return 0.3
	default:
		return 0.5
	}
}

func startsWithAny(s string, prefixes ...string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func attentionContainsAny(s string, words ...string) bool {
	for _, w := range words {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}

// sortSuggestedReadsByAttention returns a new slice with the highest-scoring
// chunks first (L-curve: model attends strongest to position 0).
// The original slice is not modified. Stable sort preserves the relative
// order of equal-scoring entries.
func sortSuggestedReadsByAttention(reads []SuggestedRead) []SuggestedRead {
	if len(reads) <= 1 {
		return reads
	}
	scored := make([]struct {
		r     SuggestedRead
		score float64
	}, len(reads))
	for i, r := range reads {
		scored[i] = struct {
			r     SuggestedRead
			score float64
		}{r, chunkImportanceScore(r.Content)}
	}
	// Insertion sort (stable, small n).
	for i := 1; i < len(scored); i++ {
		for j := i; j > 0 && scored[j].score > scored[j-1].score; j-- {
			scored[j], scored[j-1] = scored[j-1], scored[j]
		}
	}
	out := make([]SuggestedRead, len(scored))
	for i, s := range scored {
		out[i] = s.r
	}
	return out
}
