package compress

import (
	"fmt"
	"strings"
)

// ── docker ───────────────────────────────────────────────────────────────────

var (
	reDockerStep   = &lazyRe{pattern: `^Step \d+/\d+`}
	reDockerArrow  = &lazyRe{pattern: `^ --->`}
	reDockerRemove = &lazyRe{pattern: `^Removing intermediate`}
)

func CompressDocker(lines []string) []string {
	var out []string
	for _, l := range lines {
		switch {
		case reDockerRemove.MatchString(l):
			// drop
		case reDockerArrow.MatchString(l):
			// drop layer hash lines unless they're the last (image id)
		default:
			out = append(out, l)
		}
	}
	_ = reDockerStep // keep for future use
	if len(out) == 0 {
		return lines
	}
	return out
}

// ── make ──────────────────────────────────────────────────────────────────────

var (
	reMakeEnter   = &lazyRe{pattern: `^make(\[\d+\])?: (Entering|Leaving) directory`}
	reMakeNothing = &lazyRe{pattern: `^make(\[\d+\])?: Nothing to be done`}
	reMakeTarget  = &lazyRe{pattern: `^make(\[\d+\])?:`}
)

func CompressMake(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		switch {
		case reMakeEnter.MatchString(l):
			// drop directory chatter
		case reMakeNothing.MatchString(l):
			// drop no-op messages
		default:
			out = append(out, l)
		}
	}
	_ = reMakeTarget
	if len(out) == 0 {
		return lines
	}
	return out
}

// ── terraform / opentofu ──────────────────────────────────────────────────────

var (
	reTFRefresh   = &lazyRe{pattern: `^.+: (Refreshing state\.\.\.|Still (creating|destroying|modifying)\.\.\.)`}
	reTFProgress  = &lazyRe{pattern: `^\s*[\d.]+s elapsed`}
	reTFModHeader = &lazyRe{pattern: `^Terraform (will|has) (perform|been working)`}
)

func CompressTerraform(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		switch {
		case reTFRefresh.MatchString(l):
			// drop state-refresh and "still creating..." heartbeat lines
		case reTFProgress.MatchString(l):
			// drop elapsed-time lines
		default:
			out = append(out, l)
		}
	}
	_ = reTFModHeader
	if len(out) == 0 {
		return lines
	}
	return out
}

// ── cmake / ninja ─────────────────────────────────────────────────────────────

var (
	reCmakeProgress = &lazyRe{pattern: `^\[\s*\d+%\]`}
	reCmakeDashes   = &lazyRe{pattern: `^-{10,}`}
	reCmakeGenerate = &lazyRe{pattern: `^-- `}
	reNinjaProgress = &lazyRe{pattern: `^\[\d+/\d+\]`}
	reNinjaWarn     = &lazyRe{pattern: `(?i)warning:`}
)

func CompressCmake(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		switch {
		case reCmakeProgress.MatchString(l):
			// drop [xx%] build progress lines
		case reCmakeDashes.MatchString(l):
			// drop separator lines
		case reNinjaProgress.MatchString(l) && !reNinjaWarn.MatchString(l):
			// drop ninja [N/M] progress lines unless they contain a warning
		default:
			out = append(out, l)
		}
	}
	_ = reCmakeGenerate
	if len(out) == 0 {
		return lines
	}
	return out
}

// ── maven / gradle ────────────────────────────────────────────────────────────

func IsMavenNoise(l string) bool {
	return reMavenDownload.MatchString(l) || reMavenProgress.MatchString(l)
}

func IsGradleNoise(l string) bool {
	return reGradleDownload.MatchString(l) || reGradleProgress.MatchString(l)
}

func CompressMaven(cmd string, lines []string) []string {
	isMaven := strings.Contains(cmd, "mvn") || strings.Contains(cmd, "mvnw")
	var filtered []string
	for _, l := range lines {
		if isMaven && IsMavenNoise(l) {
			continue
		}
		if !isMaven && IsGradleNoise(l) {
			continue
		}
		filtered = append(filtered, l)
	}
	// Look for test summary lines.
	var testResults []string
	var buildStatus string
	for _, l := range filtered {
		if m := reMavenTestsRun.FindStringSubmatch(l); m != nil {
			testResults = append(testResults, fmt.Sprintf("Tests: run=%s fail=%s err=%s skip=%s", m[1], m[2], m[3], m[4]))
		}
		if strings.Contains(l, "BUILD SUCCESS") || strings.Contains(l, "BUILD FAILURE") {
			buildStatus = strings.TrimSpace(l)
		}
	}
	if buildStatus == "" && len(testResults) == 0 {
		return CompactLines(filtered, 30)
	}
	var out []string
	if buildStatus != "" {
		out = append(out, buildStatus)
	}
	out = append(out, testResults...)
	return out
}

// ── bazel ─────────────────────────────────────────────────────────────────────

func CompressBazel(cmd string, lines []string) []string {
	switch {
	case strings.Contains(cmd, "test"):
		return CompressBazelTest(lines)
	case strings.Contains(cmd, "build"):
		return CompressBazelBuild(lines)
	case strings.Contains(cmd, "query"):
		return CompressBazelQuery(lines)
	}
	return CompactLines(lines, 20)
}

func CompressBazelTest(lines []string) []string {
	var passed, failed int
	var failures []string
	var infos []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "PASSED:") || strings.Contains(t, ": PASSED") {
			passed++
		} else if strings.HasPrefix(t, "FAILED:") || strings.Contains(t, ": FAILED") {
			failed++
			failures = append(failures, t)
		}
		if strings.Contains(t, "INFO:") && (strings.Contains(t, "Build completed") || strings.Contains(t, "elapsed")) {
			infos = append(infos, t)
		}
	}
	if passed == 0 && failed == 0 {
		return CompactLines(lines, 15)
	}
	result := fmt.Sprintf("bazel test: %d passed, %d failed", passed, failed)
	out := []string{result}
	for i, f := range failures {
		if i >= 10 {
			out = append(out, fmt.Sprintf("  … +%d more", len(failures)-10))
			break
		}
		out = append(out, "  "+f)
	}
	if len(infos) > 0 {
		out = append(out, infos[len(infos)-1])
	}
	return out
}

func CompressBazelBuild(lines []string) []string {
	var errors []string
	var infos []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.Contains(t, "ERROR:") || strings.Contains(t, "error:") {
			errors = append(errors, t)
		}
		if strings.HasPrefix(t, "INFO:") && (strings.Contains(t, "Build completed") || strings.Contains(t, "elapsed")) {
			infos = append(infos, t)
		}
	}
	if len(errors) == 0 {
		result := "Build ok"
		if len(infos) > 0 {
			result += ": " + infos[len(infos)-1]
		}
		return []string{result}
	}
	out := []string{fmt.Sprintf("%d errors", len(errors))}
	for i, e := range errors {
		if i >= 10 {
			out = append(out, fmt.Sprintf("  … +%d more", len(errors)-10))
			break
		}
		out = append(out, "  "+e)
	}
	return out
}

func CompressBazelQuery(lines []string) []string {
	if len(lines) <= 30 {
		return lines
	}
	// Build a fresh slice rather than append(lines[:20], …): lines[:20]
	// keeps lines' backing array (cap ≥ len(lines)), so the append would
	// overwrite lines[20] in place — mutating the caller's input and
	// returning a slice that aliases it (#454).
	out := make([]string, 0, 21)
	out = append(out, lines[:20]...)
	return append(out, fmt.Sprintf("… +%d more results", len(lines)-20))
}
