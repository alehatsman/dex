package compress

import (
	"fmt"
	"strings"
)

// ── pytest / jest / vitest ────────────────────────────────────────────────────

var (
	rePytestSummary = &lazyRe{pattern: `=+\s+(\d+) (failed|passed|error)`}
	rePytestFailed  = &lazyRe{pattern: `^FAILED\s+`}
)

func CompressPytest(lines []string) []string {
	var summaryLine string
	var failures []string
	for _, l := range lines {
		if rePytestSummary.MatchString(l) {
			summaryLine = strings.TrimSpace(l)
		}
		if rePytestFailed.MatchString(l) {
			failures = append(failures, strings.TrimSpace(l))
		}
	}
	if summaryLine == "" {
		return lines
	}
	out := []string{summaryLine}
	for i, f := range failures {
		if i >= 10 {
			out = append(out, fmt.Sprintf("  … +%d more failures", len(failures)-10))
			break
		}
		out = append(out, "  "+f)
	}
	return out
}

// ── playwright / cypress ──────────────────────────────────────────────────────

var rePwFailed = &lazyRe{pattern: `^\s+\d+\)\s+`}

func CompressPlaywright(cmd string, lines []string) []string {
	var passed, failed, skipped int
	var duration string
	var failedTests []string

	for _, l := range lines {
		t := strings.TrimSpace(l)
		// Cypress-style: "3 passing (2s)"
		if strings.Contains(t, "passing") && !strings.Contains(t, "tests") {
			if m := extractNumberBefore(t, "passing"); m > 0 {
				passed = m
			}
		}
		// Playwright-style: "42 passed (30s)"
		if strings.Contains(t, "passed") {
			if m := extractNumberBefore(t, "passed"); m > 0 {
				passed = m
			}
		}
		if strings.Contains(t, "failed") {
			if m := extractNumberBefore(t, "failed"); m > 0 {
				failed = m
			}
		}
		if strings.Contains(t, "skipped") || strings.Contains(t, "pending") {
			if m := extractNumberBefore(t, "skipped"); m > 0 {
				skipped = m
			}
			if m := extractNumberBefore(t, "pending"); m > 0 {
				skipped = m
			}
		}
		if strings.Contains(t, "Finished in") || strings.Contains(t, "Duration:") {
			duration = t
		}
		if rePwFailed.MatchString(l) {
			failedTests = append(failedTests, strings.TrimSpace(l))
		}
	}

	_ = cmd
	summary := fmt.Sprintf("%d passed", passed)
	if failed > 0 {
		summary += fmt.Sprintf(", %d failed", failed)
	}
	if skipped > 0 {
		summary += fmt.Sprintf(", %d skipped", skipped)
	}
	out := []string{summary}
	if duration != "" {
		out = append(out, "  "+duration)
	}
	if len(failedTests) > 0 {
		out = append(out, "failed:")
		for i, f := range failedTests {
			if i >= 10 {
				out = append(out, fmt.Sprintf("  … +%d more", len(failedTests)-10))
				break
			}
			out = append(out, "  "+f)
		}
	}
	return out
}

func CompressCypress(lines []string) []string {
	return CompressPlaywright("cypress", lines)
}

func extractNumberBefore(s, word string) int {
	idx := strings.Index(s, word)
	if idx <= 0 {
		return 0
	}
	before := strings.TrimSpace(s[:idx])
	parts := strings.Fields(before)
	if len(parts) == 0 {
		return 0
	}
	return extractFirstInt(parts[len(parts)-1])
}

func extractFirstInt(s string) int {
	n := 0
	started := false
	for _, c := range s {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
			started = true
		} else if started {
			break
		}
	}
	return n
}
