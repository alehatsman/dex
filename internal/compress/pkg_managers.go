package compress

import (
	"fmt"
	"regexp"
	"strings"
)

// ── cargo ────────────────────────────────────────────────────────────────────

var (
	reCargoCompiling = &lazyRe{pattern: `^\s*Compiling\s`}
	reCargoChecking  = &lazyRe{pattern: `^\s*Checking\s`}
	reCargoDownload  = &lazyRe{pattern: `^\s*Downloading|^\s*Downloaded\s`}
)

func CompressCargo(lines []string) []string {
	var out []string
	compilingCount := 0
	for _, l := range lines {
		switch {
		case reCargoCompiling.MatchString(l) || reCargoChecking.MatchString(l):
			compilingCount++
			// keep first and every 10th to show progress without flooding
			if compilingCount == 1 || compilingCount%10 == 0 {
				out = append(out, l)
			}
		case reCargoDownload.MatchString(l):
			// suppress download noise
		default:
			out = append(out, l)
		}
	}
	if len(out) == 0 {
		return lines
	}
	return out
}

// ── npm / yarn / bun / pnpm ──────────────────────────────────────────────────

var (
	reNpmProgress = &lazyRe{pattern: `^\s*(npm warn|npm notice|[⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏]|▐|▌|\[[\d.]+s\])`}
	reNpmAdded    = &lazyRe{pattern: `added \d+ package`}
	reNpmErr      = &lazyRe{pattern: `(?i)^(npm ERR!|error\b|warn\b)`}
)

func CompressNpm(lines []string) []string {
	var out []string
	for _, l := range lines {
		switch {
		case reNpmProgress.MatchString(l):
			// drop progress / spinner lines
		case reNpmAdded.MatchString(l), reNpmErr.MatchString(l):
			out = append(out, l)
		case strings.TrimSpace(l) == "":
			// will be collapsed by generic pass
			out = append(out, l)
		default:
			out = append(out, l)
		}
	}
	return out
}

// ── pip / uv ──────────────────────────────────────────────────────────────────

var (
	rePipCollect  = &lazyRe{pattern: `^(Collecting|Downloading|Using cached|Obtaining)\s`}
	rePipProgress = &lazyRe{pattern: `^\s+\d+%\|`}
	rePipDepSolve = &lazyRe{pattern: `^(Requirement already satisfied|Looking in indexes)`}
	rePipResolvUV = &lazyRe{pattern: `^\s+(Resolved|Downloaded|Prepared|Installed|Uninstalled)\s`}
)

func CompressPip(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		switch {
		case rePipCollect.MatchString(l):
			// drop per-package download lines
		case rePipProgress.MatchString(l):
			// drop progress bars
		case rePipDepSolve.MatchString(l):
			// drop "Requirement already satisfied" noise
		case rePipResolvUV.MatchString(l):
			// drop uv per-package resolution lines
		default:
			out = append(out, l)
		}
	}
	if len(out) == 0 {
		return lines
	}
	return out
}

// ── poetry ────────────────────────────────────────────────────────────────────

var (
	// poetry progress lines are "  • Installing foo (1.0)" /
	// "  • Updating bar (1.0 -> 2.0)" — anchor to the leading bullet so an
	// arbitrary "Installing" / "Updating" substring elsewhere isn't dropped.
	rePoetryProgress = regexp.MustCompile(`(?i)^\s*[•·*\-]\s+(Installing|Updating)\s`)
	// poetry appends a status after the colon ("  • Installing foo (1.0):
	// Failed"); a line reporting Failed/Error is a diagnostic, never noise.
	rePoetryStatusFail = regexp.MustCompile(`(?i):\s*(failed|error)`)
	// progress-bar percentages lead the line ("  42%"), not an arbitrary
	// "12%" mention inside a diagnostic message.
	rePoetryPercentBar = regexp.MustCompile(`^\s*\d+%`)
)

func IsPoetryDownloadNoise(l string) bool {
	if rePoetryStatusFail.MatchString(l) {
		return false // keep failure headers even on a progress-shaped line
	}
	return rePoetryProgress.MatchString(l) || rePoetryPercentBar.MatchString(l)
}

func CompressPoetry(cmd string, lines []string) []string {
	_ = cmd
	var out []string
	installCount := 0
	for _, l := range lines {
		if IsPoetryDownloadNoise(l) {
			installCount++
			continue
		}
		out = append(out, l)
	}
	if installCount > 0 {
		out = append([]string{fmt.Sprintf("[%d packages installed/updated]", installCount)}, out...)
	}
	if len(out) == 0 {
		return lines
	}
	return out
}
