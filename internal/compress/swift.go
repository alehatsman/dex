package compress

import (
	"fmt"
	"strings"
)

// ── swift build ───────────────────────────────────────────────────────────────

func CompressSwiftBuild(cmd string, lines []string) []string {
	joined := strings.Join(lines, "\n")
	if strings.TrimSpace(joined) == "" {
		return []string{"ok"}
	}
	switch {
	case strings.Contains(cmd, "test"):
		return CompressSwiftTest(lines)
	case strings.Contains(cmd, "build"):
		return CompressSwiftBuildOutput(lines)
	case strings.Contains(cmd, "package resolve") || strings.Contains(cmd, "package update"):
		return CompressSwiftResolve(lines)
	}
	return CompactLines(lines, 15)
}

// reSwiftDiag matches an XCTest/swift-testing diagnostic — the line that says
// *why* a test failed. XCTest prints `file:line: error: … : XCTAssertEqual failed: …`
// (immediately BEFORE the `Test Case … failed` line); swift-testing prints
// `✘`/`recorded an issue`/`Expectation failed`. These carry the reason.
var reSwiftDiag = &lazyRe{pattern: `:\s*error:|recorded an issue|Expectation failed|XCTAssert\w* failed`}

func CompressSwiftTest(lines []string) []string {
	var passed, failed int
	var failures []string
	var diagnostics []string
	var timeStr string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.Contains(t, "Test Case") && strings.Contains(t, "passed") {
			passed++
		} else if strings.Contains(t, "Test Case") && strings.Contains(t, "failed") {
			failed++
			failures = append(failures, t)
		}
		// Retain the diagnostic reason line (#455). XCTest prints it before the
		// `failed` summary line, so collect it independently of the test boundary.
		if reSwiftDiag.MatchString(t) {
			diagnostics = append(diagnostics, t)
		}
		if strings.HasPrefix(t, "Test Suite") && strings.Contains(t, "Executed") {
			timeStr = t
		}
		if strings.Contains(t, "Executed") && strings.Contains(t, "tests") && timeStr == "" {
			if idx := strings.Index(t, "Executed"); idx >= 0 {
				timeStr = t[idx:]
			}
		}
	}
	if passed == 0 && failed == 0 {
		return CompactLines(lines, 10)
	}
	result := fmt.Sprintf("swift test: %d passed", passed)
	if failed > 0 {
		result += fmt.Sprintf(", %d failed", failed)
	}
	out := []string{result}
	if timeStr != "" {
		out = append(out, "  "+timeStr)
	}
	for i, f := range failures {
		if i >= 5 {
			break
		}
		out = append(out, "  FAIL: "+f)
	}
	for i, d := range diagnostics {
		if i >= 10 {
			out = append(out, fmt.Sprintf("  … +%d more", len(diagnostics)-10))
			break
		}
		out = append(out, "  "+d)
	}
	return out
}

func CompressSwiftBuildOutput(lines []string) []string {
	var compiling, warnings int
	linking := false
	var errors []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "Compiling") || (strings.Contains(t, "[") && strings.Contains(t, "]")) {
			compiling++
		}
		if strings.HasPrefix(t, "Linking") || strings.Contains(t, "Linking") {
			linking = true
		}
		if strings.Contains(t, "error:") {
			errors = append(errors, t)
		}
		if strings.Contains(t, "warning:") {
			warnings++
		}
	}
	if len(errors) > 0 {
		result := fmt.Sprintf("%d errors", len(errors))
		if warnings > 0 {
			result += fmt.Sprintf(", %d warnings", warnings)
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
	result := fmt.Sprintf("Build ok (%d compiled", compiling)
	if linking {
		result += ", linked"
	}
	if warnings > 0 {
		result += fmt.Sprintf(", %d warnings", warnings)
	}
	result += ")"
	return []string{result}
}

func CompressSwiftResolve(lines []string) []string {
	var fetched, resolved int
	for _, l := range lines {
		if strings.Contains(l, "Fetching") {
			fetched++
		}
		if strings.Contains(l, "Resolving") || strings.Contains(l, "resolved") {
			resolved++
		}
	}
	if fetched == 0 && resolved == 0 {
		return CompactLines(lines, 5)
	}
	return []string{fmt.Sprintf("%d fetched, %d resolved", fetched, resolved)}
}
