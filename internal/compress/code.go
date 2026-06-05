package compress

import (
	"fmt"
	"strings"
)

// AggressiveCompress strips comments, normalizes indentation, and collapses
// structural noise from source code. ext is the file extension (e.g. ".go").
// Returns original content if compression degrades quality (SafeguardRatio).
func AggressiveCompress(content, ext string) string {
	lines := strings.Split(content, "\n")
	lines = stripComments(lines, ext)
	lines = collapseClosingBracesAggressively(lines)
	if len(lines) > 200 {
		lines = halveIndentation(lines)
	}
	lines = normalizeBlankLines(lines)
	return SafeguardRatio(content, strings.Join(lines, "\n"))
}

// LightweightCleanup applies conservative cleanup safe for any file content:
// collapses long closing-brace runs (5+) and normalizes consecutive blank lines.
// Returns original content if compression degrades quality (SafeguardRatio).
func LightweightCleanup(content string) string {
	lines := strings.Split(content, "\n")
	if len(lines) > 200 {
		lines = collapseClosingBracesLightweight(lines)
	}
	lines = normalizeBlankLines(lines)
	return SafeguardRatio(content, strings.Join(lines, "\n"))
}

// SafeguardRatio returns compressed unless: compressed is no smaller than
// original, or savings are below 5% for small inputs (<2000 tokens).
func SafeguardRatio(original, compressed string) string {
	if len(compressed) >= len(original) {
		return original
	}
	if countTokens(original) < 2000 {
		saved := float64(len(original)-len(compressed)) / float64(len(original))
		if saved < 0.05 {
			return original
		}
	}
	return compressed
}

// isClosingBraceLine reports whether a line is a pure closing-brace token.
func isClosingBraceLine(line string) bool {
	t := strings.TrimSpace(line)
	return t == "}" || t == "};" || t == ");" || t == "});"
}

// collapseClosingBracesAggressively merges consecutive closing-brace-only
// lines into a single space-joined line.
func collapseClosingBracesAggressively(lines []string) []string {
	out := make([]string, 0, len(lines))
	i := 0
	for i < len(lines) {
		if !isClosingBraceLine(lines[i]) {
			out = append(out, lines[i])
			i++
			continue
		}
		j := i + 1
		for j < len(lines) && isClosingBraceLine(lines[j]) {
			j++
		}
		if j-i > 1 {
			run := make([]string, j-i)
			for k := i; k < j; k++ {
				run[k-i] = strings.TrimSpace(lines[k])
			}
			out = append(out, strings.Join(run, " "))
		} else {
			out = append(out, lines[i])
		}
		i = j
	}
	return out
}

// collapseClosingBracesLightweight collapses runs of 5+ closing-brace-only
// lines: keeps the first two and annotates the rest.
func collapseClosingBracesLightweight(lines []string) []string {
	out := make([]string, 0, len(lines))
	i := 0
	for i < len(lines) {
		if !isClosingBraceLine(lines[i]) {
			out = append(out, lines[i])
			i++
			continue
		}
		j := i + 1
		for j < len(lines) && isClosingBraceLine(lines[j]) {
			j++
		}
		run := j - i
		if run >= 5 {
			out = append(out, lines[i], lines[i+1])
			out = append(out, fmt.Sprintf("[%d brace-only lines collapsed]", run-2))
		} else {
			out = append(out, lines[i:j]...)
		}
		i = j
	}
	return out
}

// normalizeBlankLines limits consecutive blank lines to at most one.
func normalizeBlankLines(lines []string) []string {
	out := make([]string, 0, len(lines))
	prevBlank := false
	for _, line := range lines {
		blank := strings.TrimSpace(line) == ""
		if blank && prevBlank {
			continue
		}
		out = append(out, line)
		prevBlank = blank
	}
	return out
}

// halveIndentation replaces 4-space leading indentation with 2-space.
func halveIndentation(lines []string) []string {
	out := make([]string, len(lines))
	for i, line := range lines {
		out[i] = halveLineIndent(line)
	}
	return out
}

func halveLineIndent(line string) string {
	spaces := 0
	for _, c := range line {
		if c != ' ' {
			break
		}
		spaces++
	}
	if spaces < 4 {
		return line
	}
	return strings.Repeat(" ", spaces/2) + line[spaces:]
}

// stripComments removes whole-line comments for recognized file extensions.
// Inline comments (code on same line as comment) are preserved.
func stripComments(lines []string, ext string) []string {
	switch ext {
	case ".go", ".js", ".ts", ".jsx", ".tsx", ".mjs", ".cjs",
		".c", ".cpp", ".cc", ".h", ".hpp",
		".java", ".cs", ".rs", ".swift", ".kt", ".scala":
		return stripCStyleComments(lines)
	case ".py", ".rb":
		return stripHashComments(lines, false)
	case ".sh", ".bash", ".zsh", ".fish":
		return stripHashComments(lines, true)
	case ".sql":
		return stripSQLComments(lines)
	case ".html", ".htm", ".xml", ".svg":
		return stripHTMLComments(lines)
	default:
		return lines
	}
}

// stripCStyleComments removes whole-line // comments (not ///), /* */ blocks,
// and * continuation lines.
func stripCStyleComments(lines []string) []string {
	out := make([]string, 0, len(lines))
	inBlock := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if inBlock {
			if strings.Contains(trimmed, "*/") {
				inBlock = false
			}
			continue
		}
		if strings.HasPrefix(trimmed, "/*") {
			if !strings.Contains(trimmed, "*/") {
				inBlock = true
			}
			continue
		}
		// // comment but not /// doc comment
		if strings.HasPrefix(trimmed, "//") && !strings.HasPrefix(trimmed, "///") {
			continue
		}
		// * continuation lines inside doc comment blocks
		if trimmed == "*" || strings.HasPrefix(trimmed, "* ") {
			continue
		}
		out = append(out, line)
	}
	return out
}

// stripHashComments removes whole-line # comments. When preserveShebang is
// true the first line is kept even if it starts with #.
func stripHashComments(lines []string, preserveShebang bool) []string {
	out := make([]string, 0, len(lines))
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			if preserveShebang && i == 0 && strings.HasPrefix(trimmed, "#!") {
				out = append(out, line)
			}
			continue
		}
		out = append(out, line)
	}
	return out
}

// stripSQLComments removes whole-line -- comments.
func stripSQLComments(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		out = append(out, line)
	}
	return out
}

// stripHTMLComments removes <!-- ... --> comment blocks (whole lines only).
func stripHTMLComments(lines []string) []string {
	out := make([]string, 0, len(lines))
	inComment := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if inComment {
			if strings.Contains(trimmed, "-->") {
				inComment = false
			}
			continue
		}
		if strings.Contains(trimmed, "<!--") {
			if !strings.Contains(trimmed, "-->") {
				inComment = true
			}
			continue
		}
		out = append(out, line)
	}
	return out
}

// TaskToMode classifies a free-text task description and returns the
// recommended file_view mode override, or "" to keep the caller's mode.
//
// Routing:
//
//	Generate / Test tasks → "aggressive" (strip comments + halve indent;
//	  code structure is all that matters, no LLM call needed)
//	All other intents   → "" (keep caller mode; LightweightCleanup already
//	  applied in full mode as of #128)
//
// A future bottleneck pass (#115) will handle FixBug/Debug/Refactor/Review
// with ratio-adjusted filtering; for now those fall through to the default.
func TaskToMode(task string) string {
	lower := strings.ToLower(task)
	if containsAny(lower, generateKWs) || containsAny(lower, testKWs) {
		return "aggressive"
	}
	return ""
}

// generateKWs matches code-generation tasks where comments are noise.
var generateKWs = []string{
	"generat", "implement", "add feat", "add the feat", "write", "creat",
	"scaffold", "build out", "code up",
}

// testKWs matches test-writing tasks where comments are noise.
var testKWs = []string{
	"add test", "write test", "unit test", "integrat test", "test case",
	"spec", "benchmark",
}

// containsAny reports whether s contains any of the given substrings.
func containsAny(s string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}
