package compress

import (
	"fmt"
	"strings"
)

// ── zig ───────────────────────────────────────────────────────────────────────

func CompressZig(cmd string, lines []string) []string {
	joined := strings.Join(lines, "\n")
	if strings.TrimSpace(joined) == "" {
		return []string{"ok"}
	}
	if strings.Contains(cmd, "test") {
		return CompressZigTest(lines)
	}
	if strings.Contains(cmd, "build") {
		return CompressZigBuild(lines)
	}
	return CompactLines(lines, 15)
}

// isZigFailHeader reports whether a (trimmed) line is a zig test failure header.
// The `expected X, found Y` / panic / stack-trace lines that follow are retained
// as detail (#455).
func isZigFailHeader(t string) bool {
	return strings.Contains(t, "FAIL") || strings.Contains(t, "test failed")
}

func CompressZigTest(lines []string) []string {
	var passed, failed int
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.Contains(t, "1/1 test") || strings.Contains(t, "test passed") {
			passed++
		}
		if isZigFailHeader(t) {
			failed++
		}
		if strings.Contains(t, "All") && strings.Contains(t, "passed") {
			parts := strings.Fields(t)
			if len(parts) >= 2 {
				if n := ParseInt(parts[1]); n > 0 {
					passed = n
				}
			}
		}
	}
	blocks := collectFailures(lines, isZigFailHeader, nil, 12)
	if passed == 0 && failed == 0 {
		return CompactLines(lines, 10)
	}
	result := fmt.Sprintf("zig test: %d passed", passed)
	if failed > 0 {
		result += fmt.Sprintf(", %d failed", failed)
	}
	return appendFailureBlocks([]string{result}, blocks, "  ", 5)
}

func CompressZigBuild(lines []string) []string {
	var errors, warnings []string
	for _, l := range lines {
		if strings.Contains(l, "error:") || strings.Contains(l, "Error") {
			errors = append(errors, strings.TrimSpace(l))
		}
		if strings.Contains(l, "warning:") {
			warnings = append(warnings, strings.TrimSpace(l))
		}
	}
	if len(errors) > 0 {
		result := fmt.Sprintf("%d errors", len(errors))
		if len(warnings) > 0 {
			result += fmt.Sprintf(", %d warnings", len(warnings))
		}
		out := []string{result}
		for i, e := range errors {
			if i >= 10 {
				break
			}
			out = append(out, "  "+e)
		}
		return out
	}
	if len(warnings) > 0 {
		return []string{fmt.Sprintf("ok (%d warnings)", len(warnings))}
	}
	return []string{"ok"}
}
