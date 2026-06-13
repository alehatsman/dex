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
	rePoetryInstalling = regexp.MustCompile(`(?i)Installing\s+\S+`)
	rePoetryUpdating   = regexp.MustCompile(`(?i)Updating\s+\S+`)
	rePoetryPercentBar = regexp.MustCompile(`\d+%`)
)

func IsPoetryDownloadNoise(l string) bool {
	return rePoetryInstalling.MatchString(l) || rePoetryUpdating.MatchString(l) ||
		rePoetryPercentBar.MatchString(l)
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
