package compress

import (
	"fmt"
	"strings"
)

// ── shared failure-diagnostic retention (#455) ────────────────────────────────
//
// Test/lint compressors keep the summary count and the failure HEADER line but
// historically dropped the indented detail that follows (assertion diff,
// expected/actual, traceback) — the part that says *why* it failed. collectFailures
// is the one shared collector: once isHeader matches a line, it RETAINS the
// subsequent detail lines (indented, or `E `/`>`-prefixed) up to perFailureBudget,
// until the next test boundary (the next header, or a non-detail flush line).

// failureBlock is one failure: its header plus the retained detail lines.
type failureBlock struct {
	header string
	detail []string
}

// isIndentedDetail reports whether l is a continuation/detail line: leading
// whitespace, or the pytest-style `E `/`>` assertion markers at column 0. This
// is the common detail rule (indented diff/traceback) — runners whose detail is
// flush-left (e.g. minitest) pass their own predicate to collectFailures.
func isIndentedDetail(l string) bool {
	if l == "" {
		return false
	}
	if l[0] == ' ' || l[0] == '\t' {
		return true
	}
	// `E ` (error) and `> ` (failing source) markers sit at column 0.
	return rePytestDetail.MatchString(l)
}

// collectFailures scans lines and returns one failureBlock per failure header
// (matched by isHeader, on the trimmed line). For each header it retains up to
// perFailureBudget following detail lines (per isDetail, on the RAW line); the
// boundary is a blank line, the next header, or any non-detail line. Headers are
// stored trimmed; detail keeps original indentation so the diff/traceback shape
// survives. A nil isDetail uses isIndentedDetail.
func collectFailures(lines []string, isHeader func(string) bool, isDetail func(string) bool, perFailureBudget int) []failureBlock {
	if isDetail == nil {
		isDetail = isIndentedDetail
	}
	var blocks []failureBlock
	cur := -1
	for _, l := range lines {
		t := strings.TrimSpace(l)
		switch {
		case isHeader(t):
			blocks = append(blocks, failureBlock{header: t})
			cur = len(blocks) - 1
		case cur >= 0 && t != "" && isDetail(l):
			if len(blocks[cur].detail) < perFailureBudget {
				blocks[cur].detail = append(blocks[cur].detail, l)
			}
		default:
			// Blank or non-detail line ends the current failure block.
			cur = -1
		}
	}
	return blocks
}

// appendFailureBlocks renders up to maxFailures blocks onto out: header prefixed
// with headerPrefix, detail lines indented under it. Returns the extended slice.
func appendFailureBlocks(out []string, blocks []failureBlock, headerPrefix string, maxFailures int) []string {
	for i, b := range blocks {
		if i >= maxFailures {
			out = append(out, fmt.Sprintf("  … +%d more", len(blocks)-maxFailures))
			break
		}
		out = append(out, headerPrefix+b.header)
		for _, d := range b.detail {
			out = append(out, "  "+strings.TrimRight(d, " \t"))
		}
	}
	return out
}

// ── pytest / jest / vitest ────────────────────────────────────────────────────

var (
	rePytestSummary = &lazyRe{pattern: `=+\s+(\d+) (failed|passed|error)`}
	rePytestFailed  = &lazyRe{pattern: `^FAILED\s+`}
	// Assertion / traceback detail: pytest prints the failing source line with a
	// `>` marker and the error itself with an `E` marker, both at column 0.
	rePytestDetail = &lazyRe{pattern: `^[E>]\s`}
)

func CompressPytest(lines []string) []string {
	var summaryLine string
	var failures []string
	var details []string
	for _, l := range lines {
		if rePytestSummary.MatchString(l) {
			summaryLine = strings.TrimSpace(l)
		}
		if rePytestFailed.MatchString(l) {
			failures = append(failures, strings.TrimSpace(l))
		}
		if rePytestDetail.MatchString(l) {
			details = append(details, l)
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
	// Keep the assertion/traceback detail so the failure reason survives, not just
	// the FAILED count (#452). Bounded so a pathological dump can't blow the budget.
	const maxDetail = 30
	for i, d := range details {
		if i >= maxDetail {
			out = append(out, fmt.Sprintf("  … +%d more detail lines", len(details)-maxDetail))
			break
		}
		out = append(out, d)
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
	// Fallback: if we recognized nothing — no pass/fail/skip count and no
	// failing-test lines — don't emit a synthetic, misleading "0 passed".
	// Return the original output so an unrecognized reporter format (version
	// drift, a different Playwright/Cypress/Jest reporter, localized output)
	// isn't silently misreported as a clean run. Matches every other
	// compressor's `if len(out)==0 { return lines }` guard.
	if passed == 0 && failed == 0 && skipped == 0 && len(failedTests) == 0 {
		return lines
	}
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
