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
	for _, l := range lines {
		switch {
		case reGoTestFail.MatchString(l), reGoTestPass.MatchString(l),
			reGoTestSkip.MatchString(l), reGoTestCovLine.MatchString(l),
			strings.HasPrefix(l, "PASS"), strings.HasPrefix(l, "exit status"),
			strings.HasPrefix(l, "?"):
			out = append(out, l)
		case strings.HasPrefix(l, "=== RUN"): //nolint:staticcheck
			// suppress RUN lines; keep only results
		}
	}
	if len(out) == 0 {
		return lines // nothing matched — return as-is
	}
	return out
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
