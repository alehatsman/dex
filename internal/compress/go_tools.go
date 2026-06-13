package compress

import "strings"

// ── go test ──────────────────────────────────────────────────────────────────

var (
	reGoTestPass    = &lazyRe{pattern: `^ok\s+`}
	reGoTestFail    = &lazyRe{pattern: `^(FAIL|---\s+FAIL|panic:|\s+Error:)`}
	reGoTestSkip    = &lazyRe{pattern: `^---\s+SKIP`}
	reGoTestCovLine = &lazyRe{pattern: `^coverage:`}
)

func CompressGoTest(lines []string) []string {
	var out []string
	// inFailBody tracks whether we're inside a failing test's diagnostic block.
	// After a `--- FAIL`/`panic:` marker we retain the indented detail lines
	// that follow (t.Errorf/t.Fatalf output, testify's Error Trace/expected/
	// actual block) — that's the *reason* a test failed, and dropping it forces
	// an uncompressed re-run. A new test boundary or any non-indented line ends
	// the block.
	inFailBody := false
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "=== RUN"): //nolint:staticcheck
			// suppress RUN lines; a new test boundary ends any failure body
			inFailBody = false
		case reGoTestFail.MatchString(l):
			out = append(out, l)
			inFailBody = true
		case reGoTestPass.MatchString(l), reGoTestSkip.MatchString(l),
			reGoTestCovLine.MatchString(l), strings.HasPrefix(l, "PASS"),
			strings.HasPrefix(l, "exit status"), strings.HasPrefix(l, "?"):
			out = append(out, l)
			inFailBody = false
		case inFailBody && isIndentedLine(l):
			// diagnostic detail line of the current failing test — keep it
			out = append(out, l)
		default:
			inFailBody = false
		}
	}
	if len(out) == 0 {
		return lines // nothing matched — return as-is
	}
	return out
}

// isIndentedLine reports whether l begins with a space or tab — go test indents
// every t.Errorf/t.Fatalf and testify diagnostic line under its test.
func isIndentedLine(l string) bool {
	return len(l) > 0 && (l[0] == ' ' || l[0] == '\t')
}

// ── go build / vet ───────────────────────────────────────────────────────────

var reGoBuildInfo = &lazyRe{pattern: `^#\s+`}

func CompressGoBuild(lines []string) []string {
	var out []string
	for _, l := range lines {
		// Keep error/warning lines; drop package-progress lines.
		if reGoBuildInfo.MatchString(l) {
			continue
		}
		out = append(out, l)
	}
	if len(out) == 0 {
		return lines
	}
	return out
}
