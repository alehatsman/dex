// Package compress provides text-compression utilities for shell-output reduction.
package compress

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/alehatsman/dex/internal/tokens"
)

// EstimateTokens returns a real BPE token count (default o200k_base) so the
// over-compression guard's ratio test triggers on the tokens the model
// actually sees rather than a whitespace-word approximation.
func EstimateTokens(s string) int { return tokens.Count(s) }

// safetyNeedles are patterns that must survive truncation.
var safetyNeedleStrs = []string{
	"CRITICAL", "FATAL", "panic", "FAILED", "unhealthy", "Exited", "OOMKilled",
	"DETACHED HEAD", "detached", "vulnerability", "CVE-", "denied", "unauthorized",
	"forbidden", "segfault", "Segmentation fault", "SIGSEGV", "SIGKILL", "killed",
	"out of memory", "stack overflow", "permission denied", "certificate", "expired",
	"corrupt", "test result:", "panicked", "assertion", "traceback", "tests run",
	"error", "warning", "failed", "passed", "passing",
}

// ExtractSafetyLines scans lines for safety needles and returns up to max
// matching lines (preserving order). Case-insensitive match.
func ExtractSafetyLines(lines []string, max int) []string {
	var out []string
	for _, l := range lines {
		if len(out) >= max {
			break
		}
		ll := strings.ToLower(l)
		for _, needle := range safetyNeedleStrs {
			if strings.Contains(ll, strings.ToLower(needle)) {
				out = append(out, l)
				break
			}
		}
	}
	return out
}

// CompressRemoveBlankLines removes all blank lines from lines.
func CompressRemoveBlankLines(lines []string) []string {
	var out []string
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

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

// ── git ──────────────────────────────────────────────────────────────────────

var (
	reGitDiffHunkParse = &lazyRe{pattern: `^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@`}
)

func CompressGit(lines []string) []string {
	if len(lines) < 80 {
		return lines
	}
	// Reformat to compact +N:/−N: notation — much denser than unified diff.
	compact := compactDiff(lines)
	if len(compact) < len(lines) {
		return compact
	}
	// Fallback: strip context lines only.
	var out []string
	for _, l := range lines {
		if len(l) > 0 && l[0] == ' ' {
			continue
		}
		out = append(out, l)
	}
	if len(out) >= len(lines) {
		return lines
	}
	return out
}

// compactDiff reformats unified diff output to lean +N:/−N: notation.
func compactDiff(lines []string) []string {
	var out []string
	var added, removed int
	oldLine, newLine := 0, 0

	for _, l := range lines {
		if len(l) == 0 {
			continue
		}
		switch {
		case strings.HasPrefix(l, "diff ") || strings.HasPrefix(l, "--- ") ||
			strings.HasPrefix(l, "+++ "):
			out = append(out, l)
		case strings.HasPrefix(l, "index ") || strings.HasPrefix(l, "new file") ||
			strings.HasPrefix(l, "deleted file") || strings.HasPrefix(l, "old mode") ||
			strings.HasPrefix(l, "new mode"):
			// skip low-signal metadata
		case reGitDiffHunkParse.MatchString(l):
			m := reGitDiffHunkParse.FindStringSubmatch(l)
			oldLine = mustAtoi(m[1])
			newLine = mustAtoi(m[2])
		case l[0] == '-':
			out = append(out, fmt.Sprintf("-%d: %s", oldLine, l[1:]))
			oldLine++
			removed++
		case l[0] == '+':
			out = append(out, fmt.Sprintf("+%d: %s", newLine, l[1:]))
			newLine++
			added++
		default: // context line (space prefix)
			oldLine++
			newLine++
		}
	}
	out = append(out, fmt.Sprintf("diff +%d/-%d lines", added, removed))
	return out
}

func mustAtoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}

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

// ── generic ──────────────────────────────────────────────────────────────────

var (
	reProgressLine = &lazyRe{pattern: `(\d+%|[\d.]+/[\d.]+\s*(MB|KB|GB)|ETA|elapsed|\[=+>?\s*\])`}
	reTimestamp    = &lazyRe{pattern: `^\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}`}
)

func CompressGeneric(lines []string) []string {
	out := make([]string, 0, len(lines))
	prev := ""
	for _, l := range lines {
		// Strip leading timestamp for duplicate comparison so that repeated
		// log lines with different timestamps are still collapsed.
		key := l
		if loc := reTimestamp.FindStringIndex(l); loc != nil {
			key = strings.TrimSpace(l[loc[1]:])
		}
		if key == prev && key != "" {
			continue
		}
		// Drop pure progress lines (spinners, percentages, progress bars).
		if reProgressLine.MatchString(l) && len(l) < 120 {
			continue
		}
		out = append(out, l)
		prev = key
	}
	return out
}

// ── kubectl ───────────────────────────────────────────────────────────────────

var (
	reKubectlHealthy  = &lazyRe{pattern: `\s+Running\s+0\s+`}
	reKubectlProgress = &lazyRe{pattern: `^(Waiting for|waiting for|Watching)`}
	reKubectlBoiler   = &lazyRe{pattern: `^(Warning: resource|kubectl\.kubernetes\.io)`}
	reKubectlLogTS    = &lazyRe{pattern: `^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}`}
)

func CompressKubectl(lines []string) []string {
	out := make([]string, 0, len(lines))
	prev := ""
	for _, l := range lines {
		switch {
		case reKubectlHealthy.MatchString(l):
			// drop fully-ready Running pods — keep only problematic ones
		case reKubectlProgress.MatchString(l):
			// drop "Waiting for deployment..." noise
		case reKubectlBoiler.MatchString(l):
			// drop annotation warnings
		default:
			// deduplicate log lines (strip timestamp for comparison)
			key := l
			if loc := reKubectlLogTS.FindStringIndex(l); loc != nil {
				key = strings.TrimSpace(l[loc[1]:])
			}
			if key != "" && key == prev {
				continue
			}
			out = append(out, l)
			prev = key
		}
	}
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

// ── gh ────────────────────────────────────────────────────────────────────────

var (
	reGhSeparator = &lazyRe{pattern: `^─+$|^═+$`}
	reGhLabel     = &lazyRe{pattern: `^(Labels|Assignees|Projects|Milestone|Reviewers):\s*$`}
)

func CompressGh(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		switch {
		case reGhSeparator.MatchString(strings.TrimSpace(l)):
			// drop decorative separators
		case reGhLabel.MatchString(l):
			// drop empty metadata fields
		default:
			out = append(out, l)
		}
	}
	if len(out) == 0 {
		return lines
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

// CollapseBlankLines collapses runs of 2+ blank lines into a single blank line.
func CollapseBlankLines(lines []string) []string {
	out := make([]string, 0, len(lines))
	blanks := 0
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			blanks++
			if blanks == 1 {
				out = append(out, l)
			}
		} else {
			blanks = 0
			out = append(out, l)
		}
	}
	return out
}

// CompactLines returns at most max non-blank lines from lines, with an ellipsis
// suffix if lines were omitted.
func CompactLines(lines []string, max int) []string {
	if len(lines) <= max {
		return lines
	}
	out := make([]string, 0, max+1)
	kept := 0
	for _, l := range lines {
		if kept >= max {
			break
		}
		out = append(out, l)
		kept++
	}
	omitted := len(lines) - kept
	out = append(out, fmt.Sprintf("[%d lines omitted]", omitted))
	return out
}

// ── grep ─────────────────────────────────────────────────────────────────────

var reGrepLine = &lazyRe{pattern: `^([^:]+):(\d+):(.*)$`}

func CompressGrep(lines []string) []string {
	type fileMatch struct {
		file  string
		count int
		lines []string
	}
	fileMap := make(map[string]*fileMatch)
	var fileOrder []string
	total := 0
	for _, l := range lines {
		m := reGrepLine.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		file := m[1]
		total++
		if _, ok := fileMap[file]; !ok {
			fileMap[file] = &fileMatch{file: file}
			fileOrder = append(fileOrder, file)
		}
		fm := fileMap[file]
		fm.count++
		if len(fm.lines) < 3 {
			fm.lines = append(fm.lines, strings.TrimSpace(m[3]))
		}
	}
	if total == 0 {
		return lines
	}
	header := fmt.Sprintf("%d matches in %dF:", total, len(fileOrder))
	out := []string{header}
	for _, file := range fileOrder {
		fm := fileMap[file]
		out = append(out, fmt.Sprintf("%s (%d):", file, fm.count))
		for _, match := range fm.lines {
			preview := match
			if len(preview) > 80 {
				preview = preview[:80] + "…"
			}
			out = append(out, "  "+preview)
		}
		if fm.count > 3 {
			out = append(out, fmt.Sprintf("  … +%d more", fm.count-3))
		}
	}
	return out
}

// ── find ─────────────────────────────────────────────────────────────────────

var reFindSkip = &lazyRe{pattern: `^find: |Permission denied|No such file`}

func CompressFind(lines []string) []string {
	var paths []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" || reFindSkip.MatchString(t) {
			continue
		}
		paths = append(paths, t)
	}
	if len(paths) == 0 {
		return lines
	}
	// Group by directory prefix.
	dirMap := make(map[string][]string)
	var dirOrder []string
	for _, p := range paths {
		dir := p
		if idx := strings.LastIndex(p, "/"); idx > 0 {
			dir = p[:idx]
		}
		if _, ok := dirMap[dir]; !ok {
			dirOrder = append(dirOrder, dir)
		}
		dirMap[dir] = append(dirMap[dir], p)
	}
	files := len(paths)
	dirs := len(dirOrder)
	header := fmt.Sprintf("%dF in %dD:", files, dirs)
	out := []string{header}
	for _, dir := range dirOrder {
		dirPaths := dirMap[dir]
		out = append(out, dir+"/")
		shown := dirPaths
		if len(shown) > 5 {
			shown = shown[:5]
		}
		for _, p := range shown {
			base := p
			if idx := strings.LastIndex(p, "/"); idx >= 0 {
				base = p[idx+1:]
			}
			out = append(out, "  "+base)
		}
		if len(dirPaths) > 5 {
			out = append(out, fmt.Sprintf("  … +%d more", len(dirPaths)-5))
		}
	}
	return out
}

// ── eslint ────────────────────────────────────────────────────────────────────

var (
	reEslintFile = &lazyRe{pattern: `^(/[^\s]+\.(ts|js|tsx|jsx|vue|svelte|mjs|cjs)|[./][^\s]+\.(ts|js|tsx|jsx|vue|svelte|mjs|cjs))$`}
	reEslintDiag = &lazyRe{pattern: `^\s+(\d+):(\d+)\s+(error|warning)\s+(.+?)\s{2,}(\S+)\s*$`}
)

func CompressEslint(lines []string) []string {
	type diagEntry struct {
		rule string
		msg  string
		loc  string
	}
	var errors, warnings []diagEntry
	var summaryLine string

	for _, l := range lines {
		if reEslintFile.MatchString(strings.TrimSpace(l)) {
			continue // skip file path lines
		}
		if m := reEslintDiag.FindStringSubmatch(l); m != nil {
			d := diagEntry{rule: m[5], msg: m[4], loc: m[1] + ":" + m[2]}
			if m[3] == "error" {
				errors = append(errors, d)
			} else {
				warnings = append(warnings, d)
			}
			continue
		}
		t := strings.TrimSpace(l)
		if strings.Contains(t, "problem") || strings.Contains(t, "error") || strings.Contains(t, "warning") {
			if strings.Contains(t, "✖") || strings.Contains(t, "✗") || strings.Contains(t, "×") ||
				strings.Contains(t, "problems") {
				summaryLine = t
			}
		}
	}

	if len(errors) == 0 && len(warnings) == 0 {
		return lines
	}

	var out []string
	if summaryLine != "" {
		out = append(out, summaryLine)
	} else {
		out = append(out, fmt.Sprintf("%d errors, %d warnings", len(errors), len(warnings)))
	}

	// Group by rule, show top rules.
	ruleCount := make(map[string]int)
	for _, e := range errors {
		ruleCount[e.rule]++
	}
	for _, e := range warnings {
		ruleCount[e.rule]++
	}
	type rulePair struct {
		rule  string
		count int
	}
	var rules []rulePair
	for r, c := range ruleCount {
		rules = append(rules, rulePair{r, c})
	}
	sort.Slice(rules, func(i, j int) bool { return rules[i].count > rules[j].count })
	for i, r := range rules {
		if i >= 5 {
			break
		}
		out = append(out, fmt.Sprintf("  %s (%d)", r.rule, r.count))
	}
	return out
}

// ── ruff ─────────────────────────────────────────────────────────────────────

var (
	reRuffIssue    = &lazyRe{pattern: `^([^:]+):(\d+):(\d+): ([A-Z]\d+) (.+)$`}
	reRuffSummary  = &lazyRe{pattern: `Found \d+ error`}
	reRuffFixed    = &lazyRe{pattern: `Fixed \d+`}
	reRuffNoIssues = &lazyRe{pattern: `All checks passed`}
)

func CompressRuff(cmd string, lines []string) []string {
	_ = cmd
	for _, l := range lines {
		if reRuffNoIssues.MatchString(l) {
			return []string{"clean"}
		}
	}
	type issueEntry struct {
		file string
		code string
		msg  string
	}
	var issues []issueEntry
	var summaryLine string
	for _, l := range lines {
		if m := reRuffIssue.FindStringSubmatch(l); m != nil {
			issues = append(issues, issueEntry{file: m[1], code: m[4], msg: m[5]})
		}
		if reRuffSummary.MatchString(l) || reRuffFixed.MatchString(l) {
			summaryLine = strings.TrimSpace(l)
		}
	}
	if len(issues) == 0 {
		return lines
	}
	header := fmt.Sprintf("%d issues", len(issues))
	if summaryLine != "" {
		header = summaryLine
	}
	out := []string{header}
	// Group by rule code.
	codeCount := make(map[string]int)
	codeExamples := make(map[string]string)
	for _, issue := range issues {
		codeCount[issue.code]++
		if _, ok := codeExamples[issue.code]; !ok {
			codeExamples[issue.code] = issue.file + ": " + issue.msg
		}
	}
	type codePair struct {
		code  string
		count int
	}
	var codes []codePair
	for c, n := range codeCount {
		codes = append(codes, codePair{c, n})
	}
	sort.Slice(codes, func(i, j int) bool { return codes[i].count > codes[j].count })
	for i, cp := range codes {
		if i >= 8 {
			break
		}
		out = append(out, fmt.Sprintf("  %s (%d): %s", cp.code, cp.count, codeExamples[cp.code]))
	}
	return out
}

// ── mypy ──────────────────────────────────────────────────────────────────────

var (
	reMypy        = &lazyRe{pattern: `^([^:]+):(\d+): (error|note|warning): (.+?) \[([^\]]+)\]$`}
	reMypySummary = &lazyRe{pattern: `^Found \d+ error`}
	reMypySuccess = &lazyRe{pattern: `^Success: no issues`}
)

func CompressMypy(lines []string) []string {
	for _, l := range lines {
		if reMypySuccess.MatchString(l) {
			return []string{"clean"}
		}
	}
	type mypyIssue struct {
		file string
		code string
		msg  string
	}
	var errors []mypyIssue
	var summaryLine string
	for _, l := range lines {
		if m := reMypy.FindStringSubmatch(l); m != nil && m[3] == "error" {
			errors = append(errors, mypyIssue{file: m[1], code: m[5], msg: m[4]})
		}
		if reMypySummary.MatchString(l) {
			summaryLine = strings.TrimSpace(l)
		}
	}
	if len(errors) == 0 && summaryLine == "" {
		return lines
	}
	out := []string{summaryLine}
	for i, e := range errors {
		if i >= 10 {
			out = append(out, fmt.Sprintf("  … +%d more", len(errors)-10))
			break
		}
		out = append(out, fmt.Sprintf("  %s [%s]: %s", e.file, e.code, e.msg))
	}
	return out
}

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

// ── tsc ───────────────────────────────────────────────────────────────────────

var (
	reTsc      = &lazyRe{pattern: `^([^(]+)\((\d+),(\d+)\): (error|warning) (TS\d+): (.+)$`}
	reTscFound = &lazyRe{pattern: `Found \d+ error`}
)

func CompressTsc(lines []string) []string {
	type tscError struct {
		file string
		code string
		msg  string
	}
	var errors []tscError
	var summaryLine string
	for _, l := range lines {
		if m := reTsc.FindStringSubmatch(l); m != nil {
			errors = append(errors, tscError{file: strings.TrimSpace(m[1]), code: m[5], msg: m[6]})
		}
		if reTscFound.MatchString(l) {
			summaryLine = strings.TrimSpace(l)
		}
	}
	if len(errors) == 0 {
		return lines
	}
	// Count files.
	fileSet := make(map[string]struct{})
	for _, e := range errors {
		fileSet[e.file] = struct{}{}
	}
	header := fmt.Sprintf("%d errors in %d files", len(errors), len(fileSet))
	if summaryLine != "" {
		header = summaryLine + fmt.Sprintf(" (in %d files)", len(fileSet))
	}
	out := []string{header}
	for i, e := range errors {
		if i >= 10 {
			out = append(out, fmt.Sprintf("  … +%d more", len(errors)-10))
			break
		}
		out = append(out, fmt.Sprintf("  %s %s: %s", e.file, e.code, e.msg))
	}
	return out
}

// ── normalizers ───────────────────────────────────────────────────────────────

var (
	reTS       = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})?`)
	reHex      = regexp.MustCompile(`\b[0-9a-f]{32,}\b`)
	reUUID     = regexp.MustCompile(`\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	reLogLevel = regexp.MustCompile(`(?i)\b(DEBUG|INFO|WARN|WARNING|ERROR|FATAL|TRACE|NOTICE)\b\s*`)
	reSep      = regexp.MustCompile(`[-_]{3,}`)
)

func NormalizeTimestamps(s string) string { return reTS.ReplaceAllString(s, "[TS]") }
func NormalizeHashes(s string) string     { return reHex.ReplaceAllString(s, "[HASH]") }
func NormalizeUUIDs(s string) string      { return reUUID.ReplaceAllString(s, "[UUID]") }

// NormalizeLineForDedup normalizes a log line for dedup comparison:
// timestamps → [TS], UUIDs → [UUID], hex hashes → [HASH], log-level tokens stripped.
func NormalizeLineForDedup(s string) string {
	s = NormalizeTimestamps(s)
	s = NormalizeUUIDs(s)
	s = NormalizeHashes(s)
	s = reLogLevel.ReplaceAllString(s, "")
	s = strings.TrimSpace(s)
	return s
}

// IsBoilerplateLine returns true for common log boilerplate that carries no
// unique information per line.
func IsBoilerplateLine(s string) bool {
	t := strings.ToLower(strings.TrimSpace(s))
	for _, b := range []string{
		"starting", "started", "stopping", "stopped", "shutting down",
		"initializing", "initialized", "listening on", "ready",
	} {
		if strings.Contains(t, b) {
			return true
		}
	}
	return false
}

// VerbatimCompact deduplicates consecutive runs of identical normalized lines,
// annotating with [Nx] run counts.
func VerbatimCompact(lines []string) []string {
	if len(lines) == 0 {
		return nil
	}
	type run struct {
		line  string
		norm  string
		count int
	}
	var runs []run
	for _, l := range lines {
		norm := NormalizeLineForDedup(l)
		if len(runs) > 0 && runs[len(runs)-1].norm == norm {
			runs[len(runs)-1].count++
		} else {
			runs = append(runs, run{line: l, norm: norm, count: 1})
		}
	}
	var out []string
	for _, r := range runs {
		if r.count == 1 {
			out = append(out, r.line)
		} else {
			out = append(out, fmt.Sprintf("[%dx] %s", r.count, r.line))
		}
	}
	return out
}

// CompressLogDedup compresses log output by normalizing and deduplicating lines.
// Returns nil if no meaningful compression was achieved.
func CompressLogDedup(lines []string) []string {
	if len(lines) < 10 {
		return nil
	}
	compacted := VerbatimCompact(lines)
	if len(compacted) >= len(lines) {
		return nil
	}
	return compacted
}

// ── log block compressor ──────────────────────────────────────────────────────

var (
	reBlockHeader  = &lazyRe{pattern: `(?i)(error|warn|fatal|panic|exception|traceback|crash)`}
	reGitCommitHdr = &lazyRe{pattern: `^commit [0-9a-f]{7,40}`}
	reGitDiffHdrB  = &lazyRe{pattern: `^(diff --git|--- a/|\+\+\+ b/|index )`}
)

// IsBlockBoundary returns true if the line looks like a structural log boundary.
func IsBlockBoundary(l string) bool {
	t := strings.TrimSpace(l)
	if t == "" || reSep.MatchString(t) {
		return true
	}
	if reGitCommitHdr.MatchString(t) || reGitDiffHdrB.MatchString(t) {
		return true
	}
	return false
}

// IsErrorLogLine returns true if the line looks like an error/warning log entry.
func IsErrorLogLine(l string) bool {
	return reBlockHeader.MatchString(l)
}

// DedupBlockLines deduplicates similar lines within a block.
func DedupBlockLines(lines []string) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, l := range lines {
		norm := NormalizeLineForDedup(l)
		if _, ok := seen[norm]; ok {
			continue
		}
		seen[norm] = struct{}{}
		out = append(out, l)
	}
	return out
}

// CompressLogBlock detects and compresses repetitive log block output.
// Returns nil if the input doesn't look like log block output.
func CompressLogBlock(lines []string) []string {
	if len(lines) < 20 {
		return nil
	}
	errorCount := 0
	for _, l := range lines {
		if IsErrorLogLine(l) {
			errorCount++
		}
	}
	// Only operate on log-heavy output (at least 20% error/warn lines).
	if float64(errorCount)/float64(len(lines)) < 0.20 {
		return nil
	}
	// Split into blocks and deduplicate each.
	var blocks [][]string
	var current []string
	for _, l := range lines {
		if IsBlockBoundary(l) && len(current) > 0 {
			blocks = append(blocks, current)
			current = nil
		}
		current = append(current, l)
	}
	if len(current) > 0 {
		blocks = append(blocks, current)
	}
	var out []string
	for _, block := range blocks {
		deduped := DedupBlockLines(block)
		out = append(out, deduped...)
	}
	if len(out) >= len(lines) {
		return nil
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

// ── next build / vite ─────────────────────────────────────────────────────────

var (
	reNextRoute     = &lazyRe{pattern: `^[┌├└─│]\s+(○|●|ƒ|λ|◐)\s+(/[^\s]*)(\s+[\d.]+\s*\w+)?`}
	reNextSize      = &lazyRe{pattern: `First Load JS`}
	reNextBuildTime = &lazyRe{pattern: `Compiled in|compiled in|Build time`}
	reViteChunk     = &lazyRe{pattern: `^(dist/|\.\/dist/).*\s+[\d.]+\s*(kB|B|MB)`}
)

func CompressNextBuild(cmd string, lines []string) []string {
	if strings.Contains(cmd, "vite") {
		return CompressViteBuild(lines)
	}
	// Check for build error.
	for _, l := range lines {
		if strings.Contains(l, "Failed to compile") || strings.Contains(l, "Build error") {
			var errors []string
			capturing := false
			for _, el := range lines {
				if strings.Contains(el, "Failed to compile") || strings.Contains(el, "Build error") {
					capturing = true
					errors = append(errors, "BUILD ERROR: "+strings.TrimSpace(el))
					continue
				}
				if capturing {
					t := strings.TrimSpace(el)
					if t != "" {
						errors = append(errors, "  "+t)
					}
				}
			}
			return errors
		}
	}

	// Extract routes.
	var routes []string
	var buildTime string
	for _, l := range lines {
		if reNextRoute.MatchString(l) {
			routes = append(routes, strings.TrimSpace(l))
		}
		if reNextBuildTime.MatchString(l) {
			buildTime = strings.TrimSpace(l)
		}
	}

	if len(routes) == 0 {
		return CompactLines(lines, 20)
	}

	out := []string{fmt.Sprintf("routes: %d", len(routes))}
	for i, r := range routes {
		if i >= 20 {
			out = append(out, fmt.Sprintf("  … +%d more", len(routes)-20))
			break
		}
		out = append(out, "  "+r)
	}
	if buildTime != "" {
		out = append(out, buildTime)
	}
	_ = reNextSize
	return out
}

func CompressViteBuild(lines []string) []string {
	var chunks []string
	var buildTime string
	modules := 0
	for _, l := range lines {
		if reViteChunk.MatchString(l) {
			chunks = append(chunks, strings.TrimSpace(l))
		}
		if strings.Contains(l, "modules transformed") {
			if n := extractFirstInt(l); n > 0 {
				modules = n
			}
		}
		if strings.Contains(l, "built in") {
			buildTime = strings.TrimSpace(l)
		}
	}
	if len(chunks) == 0 {
		return CompactLines(lines, 20)
	}
	header := "built"
	if modules > 0 {
		header = fmt.Sprintf("built (%d modules)", modules)
	}
	if buildTime != "" {
		header += " — " + buildTime
	}
	out := []string{header, fmt.Sprintf("chunks: %d", len(chunks))}
	for i, c := range chunks {
		if i >= 10 {
			out = append(out, fmt.Sprintf("  … +%d more", len(chunks)-10))
			break
		}
		out = append(out, "  "+c)
	}
	return out
}

// ── helm ─────────────────────────────────────────────────────────────────────

var (
	reMavenDownload  = regexp.MustCompile(`(?i)(Downloading|Downloaded)\s+from\s+`)
	reMavenProgress  = regexp.MustCompile(`\d+/\d+\s*(KB|MB|B)`)
	reGradleDownload = regexp.MustCompile(`(?i)Download\s+http`)
	reGradleProgress = regexp.MustCompile(`>\s+\d+%`)
	reMavenTestsRun  = regexp.MustCompile(`Tests run:\s*(\d+),\s*Failures:\s*(\d+),\s*Errors:\s*(\d+),\s*Skipped:\s*(\d+)`)
)

func CompressHelm(cmd string, lines []string) []string {
	switch {
	case strings.Contains(cmd, "list") || strings.Contains(cmd, "ls"):
		return CompressHelmList(lines)
	case strings.Contains(cmd, "install") || strings.Contains(cmd, "upgrade"):
		return CompressHelmInstall(lines)
	case strings.Contains(cmd, "status"):
		return CompressHelmStatus(lines)
	case strings.Contains(cmd, "template"):
		return CompressHelmTemplate(lines)
	}
	return CompactLines(lines, 20)
}

func CompressHelmList(lines []string) []string {
	if len(lines) <= 20 {
		return lines
	}
	out := []string{fmt.Sprintf("%d releases:", len(lines)-1)}
	for i, l := range lines {
		if i == 0 {
			out = append(out, l) // header
			continue
		}
		if i >= 21 {
			out = append(out, fmt.Sprintf("  … +%d more", len(lines)-21))
			break
		}
		out = append(out, l)
	}
	return out
}

func CompressHelmInstall(lines []string) []string {
	var statusLines []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "NAME:") || strings.HasPrefix(t, "STATUS:") ||
			strings.HasPrefix(t, "LAST DEPLOYED:") || strings.HasPrefix(t, "NAMESPACE:") ||
			strings.HasPrefix(t, "REVISION:") || strings.Contains(t, "deployed") ||
			strings.Contains(t, "NOTES:") || strings.HasPrefix(t, "Error") {
			statusLines = append(statusLines, l)
		}
	}
	if len(statusLines) == 0 {
		return CompactLines(lines, 15)
	}
	return statusLines
}

func CompressHelmStatus(lines []string) []string {
	var out []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "NAME:") || strings.HasPrefix(t, "STATUS:") ||
			strings.HasPrefix(t, "LAST DEPLOYED:") || strings.HasPrefix(t, "NAMESPACE:") ||
			strings.HasPrefix(t, "REVISION:") || strings.HasPrefix(t, "NOTES:") {
			out = append(out, l)
		}
	}
	if len(out) == 0 {
		return CompactLines(lines, 15)
	}
	return out
}

func CompressHelmTemplate(lines []string) []string {
	// Count Kubernetes resources.
	kindCount := make(map[string]int)
	var kindOrder []string
	for _, l := range lines {
		if strings.HasPrefix(l, "kind:") {
			kind := strings.TrimSpace(strings.TrimPrefix(l, "kind:"))
			if _, ok := kindCount[kind]; !ok {
				kindOrder = append(kindOrder, kind)
			}
			kindCount[kind]++
		}
	}
	if len(kindCount) == 0 {
		return CompactLines(lines, 30)
	}
	out := []string{fmt.Sprintf("%d resources:", len(lines))}
	for _, k := range kindOrder {
		out = append(out, fmt.Sprintf("  %s: %d", k, kindCount[k]))
	}
	return out
}

// ── ansible ───────────────────────────────────────────────────────────────────

func CompressAnsible(lines []string) []string {
	joined := strings.Join(lines, "\n")
	if strings.TrimSpace(joined) == "" {
		return []string{"ok"}
	}
	if strings.Contains(joined, "PLAY RECAP") || strings.Contains(joined, "TASK [") {
		return CompressAnsiblePlaybook(lines)
	}
	return CompressAnsibleTasks(lines)
}

func CompressAnsiblePlaybook(lines []string) []string {
	var recap []string
	var failures []string
	inRecap := false
	for _, l := range lines {
		if strings.Contains(l, "PLAY RECAP") {
			inRecap = true
			continue
		}
		if inRecap {
			if strings.TrimSpace(l) != "" {
				recap = append(recap, l)
			}
			continue
		}
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "fatal:") || strings.HasPrefix(t, "FAILED!") ||
			strings.Contains(t, "UNREACHABLE!") {
			failures = append(failures, t)
		}
	}
	var out []string
	if len(recap) > 0 {
		out = append(out, "PLAY RECAP:")
		out = append(out, recap...)
	}
	if len(failures) > 0 {
		out = append(out, "failures:")
		for i, f := range failures {
			if i >= 5 {
				out = append(out, fmt.Sprintf("  … +%d more", len(failures)-5))
				break
			}
			out = append(out, "  "+f)
		}
	}
	if len(out) == 0 {
		return CompactLines(lines, 20)
	}
	return out
}

func CompressAnsibleTasks(lines []string) []string {
	var ok, changed, failed int
	for _, l := range lines {
		t := strings.ToLower(strings.TrimSpace(l))
		if strings.HasPrefix(t, "ok:") {
			ok++
		} else if strings.HasPrefix(t, "changed:") {
			changed++
		} else if strings.HasPrefix(t, "fatal:") || strings.HasPrefix(t, "failed:") {
			failed++
		}
	}
	if ok == 0 && changed == 0 && failed == 0 {
		return CompactLines(lines, 10)
	}
	result := fmt.Sprintf("ok=%d changed=%d failed=%d", ok, changed, failed)
	return []string{result}
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

func CompressGradle(lines []string) []string {
	return CompressMaven("gradle", lines)
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
	return append(lines[:20], fmt.Sprintf("… +%d more results", len(lines)-20))
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

// ── prisma ────────────────────────────────────────────────────────────────────

var (
	rePrismaBlockChars = regexp.MustCompile(`[▸▹►▻▶►]`)
)

func CompressPrisma(cmd string, lines []string) []string {
	switch {
	case strings.Contains(cmd, "generate"):
		return CompressPrismaGenerate(lines)
	case strings.Contains(cmd, "migrate"):
		return CompressPrismaMigrate(lines)
	case strings.Contains(cmd, "db push") || strings.Contains(cmd, "db pull"):
		return CompressPrismaDBSync(lines)
	}
	return CompressPrismaStripNoise(lines)
}

func CompressPrismaGenerate(lines []string) []string {
	var out []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if rePrismaBlockChars.MatchString(t) || strings.Contains(t, "Generating") ||
			strings.Contains(t, "Generated Prisma Client") || strings.Contains(t, "Start by importing") ||
			strings.Contains(t, "import { PrismaClient }") {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return CompactLines(lines, 10)
	}
	return out
}

func CompressPrismaMigrate(lines []string) []string {
	var out []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.Contains(t, "migration") || strings.Contains(t, "Migration") ||
			strings.Contains(t, "applied") || strings.Contains(t, "created") ||
			strings.Contains(t, "Your database is now in sync") || strings.HasPrefix(t, "Error") {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return CompactLines(lines, 10)
	}
	return out
}

func CompressPrismaDBSync(lines []string) []string {
	var out []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.Contains(t, "in sync") || strings.Contains(t, "changes") ||
			strings.Contains(t, "created") || strings.HasPrefix(t, "Error") {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return CompactLines(lines, 10)
	}
	return out
}

func CompressPrismaStripNoise(lines []string) []string {
	var out []string
	for _, l := range lines {
		if !rePrismaBlockChars.MatchString(l) {
			out = append(out, l)
		}
	}
	if len(out) == 0 {
		return lines
	}
	return out
}

// ── prettier ──────────────────────────────────────────────────────────────────

func CompressPrettier(lines []string) []string {
	var written, unchanged, errors int
	var errorLines []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasSuffix(t, "(unchanged)") {
			unchanged++
		} else if strings.HasSuffix(t, "(written)") || strings.Contains(t, "ms") {
			written++
		} else if strings.HasPrefix(t, "[error]") || strings.HasPrefix(t, "Error") {
			errors++
			errorLines = append(errorLines, t)
		}
	}
	if written == 0 && unchanged == 0 && errors == 0 {
		return lines
	}
	result := fmt.Sprintf("prettier: %d written, %d unchanged", written, unchanged)
	if errors > 0 {
		result += fmt.Sprintf(", %d errors", errors)
	}
	out := []string{result}
	for i, e := range errorLines {
		if i >= 5 {
			break
		}
		out = append(out, "  "+e)
	}
	return out
}

// ── ruby / rubocop ────────────────────────────────────────────────────────────

// CopEntry holds aggregated rubocop offense data for a single cop.
type CopEntry struct {
	name  string
	count int
	files []string
}

func CompressRuby(cmd string, lines []string) []string {
	switch {
	case strings.Contains(cmd, "rubocop"):
		return CompressRubocop(lines)
	case strings.Contains(cmd, "bundle"):
		return CompressBundle(lines)
	case strings.Contains(cmd, "minitest") || strings.Contains(cmd, "test") || strings.Contains(cmd, "rspec"):
		return CompressMinitest(lines)
	}
	return CompactLines(lines, 20)
}

func CompressRubocop(lines []string) []string {
	cops := make(map[string]*CopEntry)
	var copOrder []string
	var summaryLine string

	for _, l := range lines {
		t := strings.TrimSpace(l)
		// Summary line like "2 files inspected, 3 offenses detected"
		if strings.Contains(t, "offense") || strings.Contains(t, "file") && strings.Contains(t, "inspected") {
			if strings.Contains(t, "inspected") {
				summaryLine = t
			}
		}
		// Offense line: "path/file.rb:10:5: C: CopName: message"
		parts := strings.SplitN(t, ": ", 3)
		if len(parts) >= 3 {
			// parts[0] = "file:line:col", parts[1] = "Severity", parts[2] = "CopName: msg"
			severity := strings.TrimSpace(parts[1])
			if severity == "C" || severity == "W" || severity == "E" || severity == "F" {
				copMsg := parts[2]
				copName := copMsg
				if idx := strings.Index(copMsg, ":"); idx > 0 {
					copName = copMsg[:idx]
				}
				file := strings.SplitN(parts[0], ":", 2)[0]
				if _, ok := cops[copName]; !ok {
					cops[copName] = &CopEntry{name: copName}
					copOrder = append(copOrder, copName)
				}
				cops[copName].count++
				if len(cops[copName].files) < 3 {
					found := false
					for _, f := range cops[copName].files {
						if f == file {
							found = true
							break
						}
					}
					if !found {
						cops[copName].files = append(cops[copName].files, file)
					}
				}
			}
		}
	}
	if len(cops) == 0 {
		return lines
	}
	var out []string
	if summaryLine != "" {
		out = append(out, summaryLine)
	}
	grouped := GroupByCop(cops, copOrder)
	out = append(out, grouped...)
	return out
}

// GroupByCop formats rubocop cop entries into summary lines.
func GroupByCop(cops map[string]*CopEntry, order []string) []string {
	var out []string
	for _, name := range order {
		c := cops[name]
		files := strings.Join(c.files, ", ")
		out = append(out, fmt.Sprintf("  %s (%d): %s", c.name, c.count, files))
	}
	return out
}

func CompressBundle(lines []string) []string {
	var installed, using int
	var errors []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "Installing") {
			installed++
		} else if strings.HasPrefix(t, "Using") {
			using++
		} else if strings.HasPrefix(t, "Gem::") || strings.HasPrefix(t, "ERROR:") {
			errors = append(errors, t)
		}
	}
	if installed == 0 && using == 0 && len(errors) == 0 {
		return lines
	}
	result := fmt.Sprintf("bundle: %d installing, %d using", installed, using)
	out := []string{result}
	for i, e := range errors {
		if i >= 5 {
			break
		}
		out = append(out, "  "+e)
	}
	return out
}

func CompressMinitest(lines []string) []string {
	var runs, failures, errors int
	var summary string
	var failureLines []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.Contains(t, "runs,") && strings.Contains(t, "assertions") {
			summary = t
		}
		if strings.HasPrefix(t, "FAIL:") || strings.HasPrefix(t, "ERROR:") {
			if strings.HasPrefix(t, "FAIL:") {
				failures++
			} else {
				errors++
			}
			failureLines = append(failureLines, t)
		}
		if strings.Contains(t, " run") && (strings.Contains(t, "failure") || strings.Contains(t, "error")) {
			if n := extractFirstInt(t); n > 0 {
				runs = n
			}
		}
	}
	_ = runs
	if summary == "" && failures == 0 && errors == 0 {
		return lines
	}
	var out []string
	if summary != "" {
		out = append(out, summary)
	} else {
		out = append(out, fmt.Sprintf("%d failures, %d errors", failures, errors))
	}
	for i, f := range failureLines {
		if i >= 5 {
			out = append(out, fmt.Sprintf("  … +%d more", len(failureLines)-5))
			break
		}
		out = append(out, "  "+f)
	}
	return out
}

// ── composer ──────────────────────────────────────────────────────────────────

func CompressComposer(cmd string, lines []string) []string {
	switch {
	case strings.Contains(cmd, "install") || strings.Contains(cmd, "update") || strings.Contains(cmd, "require"):
		return CompressComposerInstall(lines)
	case strings.Contains(cmd, "outdated"):
		return CompressComposerOutdated(lines)
	}
	return CompactLines(lines, 15)
}

func CompressComposerInstall(lines []string) []string {
	var installed, updated int
	var errors []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		ll := strings.ToLower(t)
		if strings.HasPrefix(ll, "- installing") || strings.HasPrefix(ll, "  - installing") {
			installed++
		} else if strings.HasPrefix(ll, "- updating") || strings.HasPrefix(ll, "  - updating") {
			updated++
		} else if strings.HasPrefix(ll, "[error]") || strings.HasPrefix(ll, "your requirements") {
			errors = append(errors, t)
		}
	}
	if installed == 0 && updated == 0 && len(errors) == 0 {
		return lines
	}
	result := fmt.Sprintf("composer: %d installed, %d updated", installed, updated)
	out := []string{result}
	for i, e := range errors {
		if i >= 5 {
			break
		}
		out = append(out, "  "+e)
	}
	return out
}

func CompressComposerOutdated(lines []string) []string {
	var packages []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t != "" && !strings.HasPrefix(t, "Legend:") && !strings.HasPrefix(t, "Color") {
			packages = append(packages, t)
		}
	}
	if len(packages) == 0 {
		return []string{"up to date"}
	}
	if len(packages) <= 20 {
		return append([]string{fmt.Sprintf("%d outdated packages:", len(packages))}, packages...)
	}
	out := []string{fmt.Sprintf("%d outdated packages (top 20):", len(packages))}
	return append(out, packages[:20]...)
}

// ── artisan ───────────────────────────────────────────────────────────────────

var (
	reArtisanMigStatus  = regexp.MustCompile(`(Ran|Pending)\s+(\S+)`)
	reArtisanRoute      = regexp.MustCompile(`(GET|POST|PUT|PATCH|DELETE|HEAD)\s+(/[^\s]*)\s+.*?(\S+@\S+|\[Closure\])`)
	reArtisanTestResult = regexp.MustCompile(`(\d+) passed(?:,\s*(\d+) failed)?`)
	reArtisanPest       = regexp.MustCompile(`PASS\s+(\d+)\s+tests?\s*,\s*FAIL\s+(\d+)|Tests:\s*(\d+) passed`)
)

func CompressArtisan(cmd string, lines []string) []string {
	switch {
	case strings.Contains(cmd, "migrate"):
		if strings.Contains(cmd, "status") {
			return CompressArtisanMigrateStatus(lines)
		}
		return CompressArtisanMigrate(lines)
	case strings.Contains(cmd, "test"):
		return CompressArtisanTest(lines)
	case strings.Contains(cmd, "route:list"):
		return CompressArtisanRoutes(lines)
	case strings.Contains(cmd, "make:"):
		return CompressArtisanMake(lines)
	case strings.Contains(cmd, "queue:"):
		return CompressArtisanQueue(lines)
	}
	return CompactLines(lines, 15)
}

func CompressArtisanMigrate(lines []string) []string {
	var migrated, rolled int
	for _, l := range lines {
		t := strings.ToLower(strings.TrimSpace(l))
		if strings.Contains(t, "migrating:") || strings.Contains(t, "migrated:") {
			migrated++
		} else if strings.Contains(t, "rolling back:") || strings.Contains(t, "rolled back:") {
			rolled++
		}
	}
	if migrated == 0 && rolled == 0 {
		return CompactLines(lines, 10)
	}
	result := fmt.Sprintf("migrated: %d, rolled back: %d", migrated, rolled)
	return []string{result}
}

func CompressArtisanMigrateStatus(lines []string) []string {
	joined := strings.Join(lines, "\n")
	matches := reArtisanMigStatus.FindAllStringSubmatch(joined, -1)
	if len(matches) == 0 {
		return CompactLines(lines, 10)
	}
	var statuses []string
	for _, m := range matches {
		prefix := "-"
		if m[1] == "Ran" {
			prefix = "+"
		}
		statuses = append(statuses, prefix+" "+strings.TrimSpace(m[2]))
	}
	ran, pending := 0, 0
	for _, s := range statuses {
		if strings.HasPrefix(s, "+") {
			ran++
		} else {
			pending++
		}
	}
	out := []string{fmt.Sprintf("%d ran, %d pending:", ran, pending)}
	shown := statuses
	if len(shown) > 10 {
		shown = shown[len(shown)-10:]
	}
	for _, s := range shown {
		out = append(out, "  "+s)
	}
	if len(statuses) > 10 {
		out = append(out, fmt.Sprintf("  ... +%d more", len(statuses)-10))
	}
	return out
}

func CompressArtisanTest(lines []string) []string {
	var passed, failed int
	var failures []string
	var timeStr string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if m := reArtisanTestResult.FindStringSubmatch(t); m != nil {
			passed = ParseInt(m[1])
			if m[2] != "" {
				failed = ParseInt(m[2])
			}
		}
		if m := reArtisanPest.FindStringSubmatch(t); m != nil {
			if m[3] != "" {
				passed = ParseInt(m[3])
			} else {
				passed = ParseInt(m[1])
				failed = ParseInt(m[2])
			}
		}
		if strings.HasPrefix(t, "FAIL") || strings.HasPrefix(t, "✕") || strings.HasPrefix(t, "×") {
			failures = append(failures, t)
		}
		if strings.Contains(t, "Time:") || strings.Contains(t, "Duration:") {
			timeStr = t
		}
	}
	status := "ok"
	if failed > 0 {
		status = "FAIL"
	}
	result := fmt.Sprintf("%s: %d passed, %d failed", status, passed, failed)
	if timeStr != "" {
		result += fmt.Sprintf(" (%s)", strings.TrimSpace(timeStr))
	}
	out := []string{result}
	for i, f := range failures {
		if i >= 10 {
			break
		}
		out = append(out, "  "+f)
	}
	return out
}

func CompressArtisanRoutes(lines []string) []string {
	joined := strings.Join(lines, "\n")
	matches := reArtisanRoute.FindAllStringSubmatch(joined, -1)
	if len(matches) == 0 {
		return CompactLines(lines, 15)
	}
	out := []string{fmt.Sprintf("%d routes:", len(matches))}
	for i, m := range matches {
		if i >= 20 {
			break
		}
		out = append(out, fmt.Sprintf("  %s %s → %s", m[1], m[2], m[3]))
	}
	if len(matches) > 20 {
		out = append(out, fmt.Sprintf("  ... +%d more", len(matches)-20))
	}
	return out
}

func CompressArtisanMake(lines []string) []string {
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.Contains(t, "created successfully") || strings.Contains(t, ".php") {
			return []string{t}
		}
	}
	return []string{"created"}
}

func CompressArtisanQueue(lines []string) []string {
	var processed, failed int
	var lastJob string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.Contains(t, "Processed") || strings.Contains(t, "[DONE]") {
			processed++
			if parts := strings.Fields(t); len(parts) > 0 {
				lastJob = parts[len(parts)-1]
			}
		}
		if strings.Contains(t, "FAILED") || strings.Contains(t, "[ERROR]") {
			failed++
		}
	}
	if processed == 0 && failed == 0 {
		return CompactLines(lines, 5)
	}
	result := fmt.Sprintf("queue: %d processed", processed)
	if failed > 0 {
		result += fmt.Sprintf(", %d failed", failed)
	}
	if lastJob != "" {
		result += fmt.Sprintf(" (last: %s)", lastJob)
	}
	return []string{result}
}

// ── mix ────────────────────────────────────────────────────────────────────────

func CompressMix(cmd string, lines []string) []string {
	joined := strings.Join(lines, "\n")
	if strings.TrimSpace(joined) == "" {
		return []string{"ok"}
	}
	switch {
	case strings.Contains(cmd, "test"):
		return CompressMixTest(lines)
	case strings.Contains(cmd, "deps.get") || strings.Contains(cmd, "deps.compile"):
		return CompressMixDeps(lines)
	case strings.Contains(cmd, "compile") || strings.Contains(cmd, "build"):
		return CompressMixCompile(lines)
	case strings.Contains(cmd, "format") || strings.Contains(cmd, "fmt"):
		var files []string
		for _, l := range lines {
			if strings.TrimSpace(l) != "" {
				files = append(files, l)
			}
		}
		if len(files) == 0 {
			return []string{"ok (formatted)"}
		}
		return []string{fmt.Sprintf("%d files", len(files))}
	case strings.Contains(cmd, "credo") || strings.Contains(cmd, "dialyzer"):
		return CompressMixLint(lines)
	}
	return CompactLines(lines, 15)
}

func CompressMixTest(lines []string) []string {
	// Find summary line (search from bottom)
	for i := len(lines) - 1; i >= 0; i-- {
		l := lines[i]
		if strings.Contains(l, "test") && (strings.Contains(l, "passed") || strings.Contains(l, "failure")) {
			result := "mix test: " + strings.TrimSpace(l)
			var failures []string
			for _, fl := range lines {
				ft := strings.TrimSpace(fl)
				if len(ft) >= 2 && ft[0] >= '1' && ft[0] <= '9' && ft[1] == ')' {
					failures = append(failures, ft)
				}
			}
			out := []string{result}
			for j, f := range failures {
				if j >= 5 {
					break
				}
				out = append(out, "  "+f)
			}
			return out
		}
	}
	return CompactLines(lines, 10)
}

func CompressMixDeps(lines []string) []string {
	var resolved, compiled int
	for _, l := range lines {
		ll := strings.ToLower(l)
		if strings.Contains(ll, "resolving") {
			resolved++
		}
		if strings.Contains(ll, "compiling") {
			compiled++
		}
	}
	if resolved == 0 && compiled == 0 {
		return CompactLines(lines, 5)
	}
	return []string{fmt.Sprintf("deps: %d resolved, %d compiled", resolved, compiled)}
}

func CompressMixCompile(lines []string) []string {
	var compiled, warnings int
	var errors []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "Compiling") || strings.HasPrefix(t, "Compiled") {
			compiled++
		}
		if strings.Contains(t, "warning:") {
			warnings++
		}
		if strings.Contains(t, "error") && strings.Contains(t, "**") {
			errors = append(errors, t)
		}
	}
	if len(errors) > 0 {
		out := []string{fmt.Sprintf("%d errors", len(errors))}
		for i, e := range errors {
			if i >= 10 {
				break
			}
			out = append(out, "  "+e)
		}
		return out
	}
	result := fmt.Sprintf("%d compiled", compiled)
	if warnings > 0 {
		result += fmt.Sprintf(", %d warnings", warnings)
	}
	return []string{result}
}

func CompressMixLint(lines []string) []string {
	var issues []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.Contains(t, "┃") || strings.HasPrefix(t, "warning:") || strings.HasPrefix(t, "error:") {
			issues = append(issues, t)
		}
	}
	if len(issues) == 0 {
		joined := strings.Join(lines, "\n")
		if strings.Contains(joined, "no issues") || strings.Contains(joined, "Analysis finished") {
			return []string{"clean"}
		}
		return CompactLines(lines, 10)
	}
	out := []string{fmt.Sprintf("%d issues:", len(issues))}
	for i, issue := range issues {
		if i >= 10 {
			break
		}
		out = append(out, "  "+issue)
	}
	return out
}

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

func CompressSwiftTest(lines []string) []string {
	var passed, failed int
	var failures []string
	var timeStr string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.Contains(t, "Test Case") && strings.Contains(t, "passed") {
			passed++
		} else if strings.Contains(t, "Test Case") && strings.Contains(t, "failed") {
			failed++
			failures = append(failures, t)
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

func CompressZigTest(lines []string) []string {
	var passed, failed int
	var failures []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.Contains(t, "1/1 test") || strings.Contains(t, "test passed") {
			passed++
		}
		if strings.Contains(t, "FAIL") || strings.Contains(t, "test failed") {
			failed++
			failures = append(failures, t)
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
	if passed == 0 && failed == 0 {
		return CompactLines(lines, 10)
	}
	result := fmt.Sprintf("zig test: %d passed", passed)
	if failed > 0 {
		result += fmt.Sprintf(", %d failed", failed)
	}
	out := []string{result}
	for i, f := range failures {
		if i >= 5 {
			break
		}
		out = append(out, "  "+f)
	}
	return out
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

// ── ps / du / ping ────────────────────────────────────────────────────────────

func CompressPs(lines []string) []string {
	if len(lines) < 2 {
		return nil
	}
	header := lines[0]
	procs := lines[1:]
	var nonEmpty []string
	for _, p := range procs {
		if strings.TrimSpace(p) != "" {
			nonEmpty = append(nonEmpty, p)
		}
	}
	if len(nonEmpty) <= 10 {
		return nil
	}
	var highCPU, highMem []string
	for _, l := range nonEmpty {
		cols := strings.Fields(l)
		if len(cols) >= 4 {
			cpu := ParseFloat(cols[2])
			mem := ParseFloat(cols[3])
			if cpu > 1.0 {
				highCPU = append(highCPU, l)
			}
			if mem > 1.0 {
				highMem = append(highMem, l)
			}
		}
	}
	out := []string{fmt.Sprintf("ps: %d processes", len(nonEmpty)), header}
	if len(highCPU) > 0 {
		out = append(out, fmt.Sprintf("--- high CPU (%d) ---", len(highCPU)))
		for i, l := range highCPU {
			if i >= 15 {
				break
			}
			out = append(out, l)
		}
	}
	if len(highMem) > 0 {
		out = append(out, fmt.Sprintf("--- high MEM (%d) ---", len(highMem)))
		for i, l := range highMem {
			if i >= 15 {
				break
			}
			out = append(out, l)
		}
	}
	return out
}

func CompressDu(lines []string) []string {
	var nonEmpty []string
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			nonEmpty = append(nonEmpty, l)
		}
	}
	if len(nonEmpty) <= 10 {
		return nil
	}
	type entry struct {
		size uint64
		path string
	}
	var entries []entry
	for _, l := range nonEmpty {
		parts := strings.SplitN(l, "\t", 2)
		if len(parts) == 2 {
			entries = append(entries, entry{ParseSizeField(strings.TrimSpace(parts[0])), strings.TrimSpace(parts[1])})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].size > entries[j].size })
	out := []string{fmt.Sprintf("du: %d entries (top 15 by size)", len(nonEmpty))}
	for i, e := range entries {
		if i >= 15 {
			break
		}
		out = append(out, fmt.Sprintf("%s\t%s", FormatDuSize(e.size), e.path))
	}
	return out
}

func CompressPing(lines []string) []string {
	if len(lines) < 3 {
		return nil
	}
	var host, stats, rtt string
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "PING ") || strings.HasPrefix(l, "ping "):
			host = l
		case strings.Contains(l, "packets transmitted") || strings.Contains(l, "packet loss"):
			stats = l
		case strings.Contains(l, "rtt ") || strings.Contains(l, "round-trip"):
			rtt = l
		}
	}
	if stats == "" {
		return nil
	}
	var out []string
	if host != "" {
		out = append(out, host)
	}
	out = append(out, stats)
	if rtt != "" {
		out = append(out, rtt)
	}
	return out
}

// ── systemd ───────────────────────────────────────────────────────────────────

func CompressSystemd(cmd string, lines []string) []string {
	joined := strings.Join(lines, "\n")
	if strings.TrimSpace(joined) == "" {
		return []string{"ok"}
	}
	if strings.HasPrefix(cmd, "systemctl") {
		return CompressSystemctl(cmd, lines)
	}
	if strings.HasPrefix(cmd, "journalctl") {
		return CompressJournal(lines)
	}
	return CompactLines(lines, 15)
}

func CompressSystemctl(cmd string, lines []string) []string {
	if strings.Contains(cmd, "status") {
		return CompressSystemctlStatus(lines)
	}
	if strings.Contains(cmd, "list-units") || strings.Contains(cmd, "list-unit-files") ||
		(!strings.Contains(cmd, "start") && !strings.Contains(cmd, "stop") &&
			!strings.Contains(cmd, "restart") && !strings.Contains(cmd, "enable") &&
			!strings.Contains(cmd, "disable")) {
		return CompressSystemctlList(lines)
	}
	return CompactLines(lines, 10)
}

func CompressSystemctlStatus(lines []string) []string {
	var parts []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "Active:") || strings.HasPrefix(t, "Loaded:") ||
			strings.HasPrefix(t, "Main PID:") || strings.HasPrefix(t, "Memory:") ||
			strings.HasPrefix(t, "CPU:") || strings.HasPrefix(t, "Tasks:") {
			parts = append(parts, t)
		}
	}
	if len(parts) == 0 {
		return CompactLines(lines, 10)
	}
	return parts
}

func CompressSystemctlList(lines []string) []string {
	var nonEmpty []string
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			nonEmpty = append(nonEmpty, l)
		}
	}
	if len(nonEmpty) <= 20 {
		return nonEmpty
	}
	stateCounts := make(map[string]int)
	for _, l := range nonEmpty[1:] {
		parts := strings.Fields(l)
		if len(parts) >= 3 {
			stateCounts[parts[2]]++
		}
	}
	header := nonEmpty[0]
	out := []string{header, fmt.Sprintf("%d units:", len(nonEmpty)-1)}
	for state, count := range stateCounts {
		out = append(out, fmt.Sprintf("  %s: %d", state, count))
	}
	return out
}

func CompressJournal(lines []string) []string {
	if len(lines) <= 30 {
		return lines
	}
	deduped := make(map[string]int)
	for _, l := range lines {
		// Strip timestamp prefix (first 3 space-separated tokens are timestamp+host+unit)
		parts := strings.SplitN(l, " ", 4)
		key := l
		if len(parts) == 4 {
			key = parts[3]
		}
		deduped[key]++
	}
	type pair struct {
		msg   string
		count int
	}
	var sorted []pair
	for msg, count := range deduped {
		sorted = append(sorted, pair{msg, count})
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].count > sorted[j].count })
	out := []string{fmt.Sprintf("%d log lines (deduped to %d):", len(lines), len(sorted))}
	for i, p := range sorted {
		if i >= 20 {
			break
		}
		if p.count > 1 {
			out = append(out, fmt.Sprintf("  (%dx) %s", p.count, p.msg))
		} else {
			out = append(out, "  "+p.msg)
		}
	}
	return out
}

// ── ls ────────────────────────────────────────────────────────────────────────

func CompressLs(lines []string) []string {
	if len(lines) < 5 {
		return nil
	}
	isLong := false
	for _, l := range lines {
		if strings.HasPrefix(l, "-") || strings.HasPrefix(l, "d") ||
			strings.HasPrefix(l, "l") || strings.HasPrefix(l, "total ") {
			isLong = true
			break
		}
	}
	if isLong {
		return CompressLsLong(lines)
	}
	return CompressLsShort(lines)
}

func CompressLsLong(lines []string) []string {
	var dirs, files []string
	for _, l := range lines {
		if strings.HasPrefix(l, "total ") || strings.TrimSpace(l) == "" {
			continue
		}
		parts := strings.Fields(l)
		if len(parts) < 9 {
			continue
		}
		name := strings.Join(parts[8:], " ")
		if name == "." || name == ".." {
			continue
		}
		if strings.HasPrefix(l, "d") {
			dirs = append(dirs, name+"/")
		} else {
			size := LsFormatSize(parts[4])
			files = append(files, fmt.Sprintf("%s  %s", name, size))
		}
	}
	if len(dirs) == 0 && len(files) == 0 {
		return nil
	}
	var out []string
	out = append(out, dirs...)
	out = append(out, files...)
	out = append(out, "")
	out = append(out, fmt.Sprintf("%d files, %d dirs", len(files), len(dirs)))
	return out
}

func CompressLsShort(lines []string) []string {
	var items []string
	for _, l := range lines {
		for _, w := range strings.Fields(l) {
			if w != "" {
				items = append(items, w)
			}
		}
	}
	if len(items) < 10 {
		return nil
	}
	var dirs, files []string
	for _, item := range items {
		if strings.HasSuffix(item, "/") {
			dirs = append(dirs, item)
		} else {
			files = append(files, item)
		}
	}
	var out []string
	out = append(out, dirs...)
	// wrap files at 70 chars
	var lineBuf strings.Builder
	for _, f := range files {
		if lineBuf.Len()+len(f)+2 > 70 {
			out = append(out, lineBuf.String())
			lineBuf.Reset()
		}
		if lineBuf.Len() > 0 {
			lineBuf.WriteString("  ")
		}
		lineBuf.WriteString(f)
	}
	if lineBuf.Len() > 0 {
		out = append(out, lineBuf.String())
	}
	out = append(out, "")
	out = append(out, fmt.Sprintf("%d items", len(dirs)+len(files)))
	return out
}

func LsFormatSize(sizeStr string) string {
	if len(sizeStr) == 0 {
		return "0B"
	}
	last := sizeStr[len(sizeStr)-1]
	if last == 'K' || last == 'M' || last == 'G' || last == 'T' {
		return sizeStr
	}
	n := ParseUint64(sizeStr)
	switch {
	case n >= 1_048_576:
		return fmt.Sprintf("%.1fM", float64(n)/1_048_576)
	case n >= 1024:
		return fmt.Sprintf("%.1fK", float64(n)/1024)
	default:
		return fmt.Sprintf("%dB", n)
	}
}

// ── mysql / psql ──────────────────────────────────────────────────────────────

func CompressMySQL(cmd string, lines []string) []string {
	joined := strings.TrimSpace(strings.Join(lines, "\n"))
	if joined == "" {
		return []string{"ok"}
	}
	if IsMySQLTableOutput(lines) {
		return CompressMySQLTable(lines)
	}
	if strings.Contains(cmd, "show databases") || strings.Contains(cmd, "show tables") {
		return CompressMySQLShow(lines)
	}
	if strings.HasPrefix(joined, "Query OK") || strings.HasPrefix(joined, "Empty set") {
		return []string{strings.Split(joined, "\n")[0]}
	}
	return CompactLines(lines, 20)
}

func IsMySQLTableOutput(lines []string) bool {
	for _, l := range lines {
		if strings.HasPrefix(l, "+") && strings.Contains(l, "---") {
			return true
		}
	}
	return false
}

func CompressMySQLTable(lines []string) []string {
	var dataLines []string
	for _, l := range lines {
		if !strings.HasPrefix(l, "+") && strings.TrimSpace(l) != "" {
			dataLines = append(dataLines, l)
		}
	}
	rowCount := 0
	if len(dataLines) > 1 {
		rowCount = len(dataLines) - 1
	}
	if rowCount <= 20 {
		return lines
	}
	// Find second separator row (end of header)
	sepCount := 0
	headerEnd := 3
	for i, l := range lines {
		if strings.HasPrefix(l, "+") {
			sepCount++
			if sepCount == 2 {
				headerEnd = i + 1
				break
			}
		}
	}
	previewEnd := headerEnd + 10
	if previewEnd > len(lines) {
		previewEnd = len(lines)
	}
	out := lines[:previewEnd]
	return append(out, fmt.Sprintf("... (%d rows total)", rowCount))
}

func CompressMySQLShow(lines []string) []string {
	var items []string
	for _, l := range lines {
		t := strings.TrimSpace(strings.Trim(l, "|"))
		t = strings.TrimSpace(t)
		if t == "" || strings.HasPrefix(l, "+") || strings.Contains(t, "---") ||
			t == "Database" || strings.HasPrefix(t, "Tables_in") {
			continue
		}
		items = append(items, t)
	}
	if len(items) == 0 {
		return []string{"empty"}
	}
	if len(items) <= 30 {
		return []string{fmt.Sprintf("%d items: %s", len(items), strings.Join(items, ", "))}
	}
	return []string{fmt.Sprintf("%d items: %s, ... +%d more",
		len(items), strings.Join(items[:20], ", "), len(items)-20)}
}

func CompressPsql(cmd string, lines []string) []string {
	joined := strings.TrimSpace(strings.Join(lines, "\n"))
	if joined == "" {
		return []string{"ok"}
	}
	if IsPsqlTableOutput(lines) {
		return CompressPsqlTable(lines)
	}
	if strings.Contains(cmd, `\dt`) || strings.Contains(cmd, `\d`) {
		return CompressPsqlDescribe(lines)
	}
	for _, prefix := range []string{"INSERT", "UPDATE", "DELETE", "CREATE", "ALTER", "DROP"} {
		if strings.HasPrefix(joined, prefix) {
			return []string{strings.Split(joined, "\n")[0]}
		}
	}
	return CompactLines(lines, 20)
}

func IsPsqlTableOutput(lines []string) bool {
	for _, l := range lines {
		if strings.Contains(l, "---+---") || strings.Contains(l, "-+-") {
			return true
		}
	}
	return false
}

func CompressPsqlTable(lines []string) []string {
	sepIdx := 0
	for i, l := range lines {
		if strings.Contains(l, "---+---") || strings.Contains(l, "-+-") {
			sepIdx = i
			break
		}
	}
	var dataRows int
	for _, l := range lines[sepIdx+1:] {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "(") {
			continue
		}
		dataRows++
	}
	// Find row count line like "(42 rows)"
	var countStr string
	for i := len(lines) - 1; i >= 0; i-- {
		t := strings.TrimSpace(lines[i])
		if strings.HasPrefix(t, "(") && strings.Contains(t, "row") {
			countStr = t
			break
		}
	}
	if countStr == "" {
		countStr = fmt.Sprintf("(%d rows)", dataRows)
	}
	if dataRows <= 20 {
		return lines
	}
	previewEnd := sepIdx + 11
	if previewEnd > len(lines) {
		previewEnd = len(lines)
	}
	out := lines[:previewEnd]
	return append(out, "... "+countStr)
}

func CompressPsqlDescribe(lines []string) []string {
	var nonEmpty []string
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			nonEmpty = append(nonEmpty, l)
		}
	}
	if len(nonEmpty) <= 30 {
		return nonEmpty
	}
	out := nonEmpty[:20]
	return append(out, fmt.Sprintf("... (%d more lines)", len(nonEmpty)-20))
}

// ── env filter ────────────────────────────────────────────────────────────────

var envSensitivePatterns = []string{
	"KEY", "SECRET", "TOKEN", "PASSWORD", "PASSWD", "CREDENTIALS",
	"AUTH", "API_KEY", "PRIVATE", "CERT",
}

func CompressEnvFilter(lines []string) []string {
	if len(lines) == 0 {
		return []string{"(empty)"}
	}
	groups := make(map[string][]string)
	var groupOrder []string
	var ungrouped []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" {
			continue
		}
		if idx := strings.Index(t, "="); idx > 0 {
			key := t[:idx]
			value := t[idx+1:]
			isSensitive := false
			keyUpper := strings.ToUpper(key)
			for _, p := range envSensitivePatterns {
				if strings.Contains(keyUpper, p) {
					isSensitive = true
					break
				}
			}
			displayVal := value
			if isSensitive {
				displayVal = "***"
			} else if len(value) > 80 {
				displayVal = value[:40] + "..."
			}
			entry := key + "=" + displayVal
			prefix := key
			if idx2 := strings.Index(key, "_"); idx2 > 0 {
				prefix = key[:idx2]
			}
			if _, exists := groups[prefix]; !exists {
				groupOrder = append(groupOrder, prefix)
			}
			groups[prefix] = append(groups[prefix], entry)
		} else {
			ungrouped = append(ungrouped, t)
		}
	}
	total := 0
	for _, v := range groups {
		total += len(v)
	}
	total += len(ungrouped)
	out := []string{fmt.Sprintf("%d variables:", total)}
	for _, prefix := range groupOrder {
		vars := groups[prefix]
		if len(vars) >= 3 {
			out = append(out, fmt.Sprintf("[%s_*] (%d vars)", prefix, len(vars)))
			for i, v := range vars {
				if i >= 3 {
					break
				}
				out = append(out, "  "+v)
			}
			if len(vars) > 3 {
				out = append(out, fmt.Sprintf("  ... +%d more", len(vars)-3))
			}
		}
	}
	// collect small groups
	var small []string
	for _, prefix := range groupOrder {
		if len(groups[prefix]) < 3 {
			small = append(small, groups[prefix]...)
		}
	}
	small = append(small, ungrouped...)
	if len(small) > 0 {
		out = append(out, fmt.Sprintf("[other] (%d vars)", len(small)))
		for i, v := range small {
			if i >= 5 {
				break
			}
			out = append(out, "  "+v)
		}
		if len(small) > 5 {
			out = append(out, fmt.Sprintf("  ... +%d more", len(small)-5))
		}
	}
	return out
}

// ── helpers ───────────────────────────────────────────────────────────────────

// CompactLinesFiltered returns at most max non-empty lines from the input slice.
// Both CompactLines and CompactLinesFiltered are in this file.
func CompactLinesFiltered(lines []string, max int) []string {
	var nonEmpty []string
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			nonEmpty = append(nonEmpty, l)
		}
	}
	if len(nonEmpty) <= max {
		return nonEmpty
	}
	return append(nonEmpty[:max], fmt.Sprintf("... (%d more lines)", len(nonEmpty)-max))
}

func ParseInt(s string) int {
	s = strings.TrimSpace(s)
	n := 0
	for _, c := range s {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		} else {
			break
		}
	}
	return n
}

func ParseFloat(s string) float64 {
	var n float64
	_, _ = fmt.Sscanf(strings.TrimSpace(s), "%f", &n) // best-effort: 0 on parse failure
	return n
}

func ParseUint64(s string) uint64 {
	var n uint64
	for _, c := range strings.TrimSpace(s) {
		if c >= '0' && c <= '9' {
			n = n*10 + uint64(c-'0')
		} else {
			break
		}
	}
	return n
}

func ParseSizeField(s string) uint64 {
	s = strings.TrimSpace(s)
	if n, err := fmt.Sscanf(s, "%d", new(uint64)); n == 1 && err == nil {
		var v uint64
		_, _ = fmt.Sscanf(s, "%d", &v) // guarded by the check above
		return v
	}
	if len(s) == 0 {
		return 0
	}
	last := s[len(s)-1]
	prefix := s[:len(s)-1]
	var base float64
	_, _ = fmt.Sscanf(prefix, "%f", &base) // best-effort: 0 on parse failure
	switch last {
	case 'K', 'k':
		return uint64(base * 1024)
	case 'M', 'm':
		return uint64(base * 1024 * 1024)
	case 'G', 'g':
		return uint64(base * 1024 * 1024 * 1024)
	}
	var v uint64
	_, _ = fmt.Sscanf(s, "%d", &v) // best-effort: 0 on parse failure
	return v
}

func FormatDuSize(bytes uint64) string {
	switch {
	case bytes >= 1_073_741_824:
		return fmt.Sprintf("%.1fG", float64(bytes)/1_073_741_824)
	case bytes >= 1_048_576:
		return fmt.Sprintf("%.1fM", float64(bytes)/1_048_576)
	case bytes >= 1024:
		return fmt.Sprintf("%.0fK", float64(bytes)/1024)
	default:
		return fmt.Sprintf("%d", bytes)
	}
}
