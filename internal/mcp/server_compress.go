package mcp

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type CompressInput struct {
	Output   string `json:"output"              jsonschema:"raw command output to compress"`
	Command  string `json:"command,omitempty"   jsonschema:"command name hint (e.g. 'go test', 'git log', 'npm install') — selects compression patterns"`
	MaxLines int    `json:"max_lines,omitempty" jsonschema:"hard cap on output lines (default 200)"`
}

type CompressOutput struct {
	Status        string `json:"status"`
	Compressed    string `json:"compressed"`
	OriginalLines int    `json:"original_lines"`
	OutputLines   int    `json:"output_lines"`
	SavedPct      int    `json:"saved_pct"`
}

// minCompressLines is the minimum number of lines required before any pattern
// runs — tiny outputs gain nothing and can only be made worse.
const minCompressLines = 5

// CompressText applies command-specific and generic compression patterns to
// output text. command is a hint (e.g. "go test", "git diff") that selects
// the pattern set; an empty or unrecognised command falls back to the generic
// pass. maxLines caps the result (0 → 200). This is the pure-text entry point
// shared by the MCP tool and the CLI compress-stdin command.
func CompressText(output, command string, maxLines int) (compressed string, originalLines, outputLines int) { //nolint:cyclop
	if output == "" {
		return "", 0, 0
	}
	if maxLines <= 0 {
		maxLines = 200
	}

	// Strip ANSI escape codes so patterns match colored terminal output.
	stripped := stripANSI(output)

	lines := strings.Split(strings.TrimRight(stripped, "\n"), "\n")
	originalLines = len(lines)

	// Skip compression for tiny outputs and auth flows.
	if originalLines < minCompressLines || containsAuthFlow(stripped) {
		return output, originalLines, originalLines
	}

	cmd := strings.ToLower(strings.TrimSpace(command))
	var out []string
	switch {
	case strings.HasPrefix(cmd, "go test"):
		out = compressGoTest(lines)
	case strings.HasPrefix(cmd, "go build") || strings.HasPrefix(cmd, "go vet"):
		out = compressGoBuild(lines)
	case strings.HasPrefix(cmd, "git"):
		out = compressGit(lines)
	case strings.HasPrefix(cmd, "cargo"):
		out = compressCargo(lines)
	case strings.HasPrefix(cmd, "npm ") || strings.HasPrefix(cmd, "yarn ") ||
		strings.HasPrefix(cmd, "bun ") || strings.HasPrefix(cmd, "pnpm ") ||
		strings.HasPrefix(cmd, "turbo ") || strings.HasPrefix(cmd, "nx "):
		out = compressNpm(lines)
	case strings.HasPrefix(cmd, "docker"):
		out = compressDocker(lines)
	case strings.HasPrefix(cmd, "kubectl"):
		out = compressKubectl(lines)
	case strings.HasPrefix(cmd, "make") || strings.HasPrefix(cmd, "gmake"):
		out = compressMake(lines)
	case strings.HasPrefix(cmd, "gh "):
		out = compressGh(lines)
	case strings.HasPrefix(cmd, "pip ") || strings.HasPrefix(cmd, "pip3 ") ||
		strings.HasPrefix(cmd, "uv ") || strings.HasPrefix(cmd, "conda ") ||
		strings.HasPrefix(cmd, "mamba ") || strings.HasPrefix(cmd, "pipx "):
		out = compressPip(lines)
	case strings.HasPrefix(cmd, "terraform") || strings.HasPrefix(cmd, "tofu"):
		out = compressTerraform(lines)
	case strings.HasPrefix(cmd, "cmake") || strings.HasPrefix(cmd, "ninja") ||
		strings.HasPrefix(cmd, "gcc ") || strings.HasPrefix(cmd, "g++ ") ||
		strings.HasPrefix(cmd, "cc "):
		out = compressCmake(lines)
	case strings.HasPrefix(cmd, "grep ") || strings.HasPrefix(cmd, "rg ") ||
		strings.HasPrefix(cmd, "ag ") || strings.HasPrefix(cmd, "ack "):
		out = compressGrep(lines)
	case strings.HasPrefix(cmd, "find ") || strings.HasPrefix(cmd, "fd "):
		out = compressFind(lines)
	case strings.HasPrefix(cmd, "eslint") || strings.HasPrefix(cmd, "npx eslint") ||
		strings.HasPrefix(cmd, "biome") || strings.HasPrefix(cmd, "hadolint") ||
		strings.HasPrefix(cmd, "yamllint") || strings.HasPrefix(cmd, "markdownlint") ||
		strings.HasPrefix(cmd, "oxlint"):
		out = compressEslint(lines)
	case strings.HasPrefix(cmd, "ruff"):
		out = compressRuff(cmd, lines)
	case strings.HasPrefix(cmd, "mypy") ||
		strings.HasPrefix(cmd, "pyright") || strings.HasPrefix(cmd, "basedpyright"):
		out = compressMypy(lines)
	case strings.HasPrefix(cmd, "pytest") || strings.HasPrefix(cmd, "python -m pytest") ||
		strings.HasPrefix(cmd, "python3 -m pytest") || strings.HasPrefix(cmd, "vitest") ||
		strings.HasPrefix(cmd, "jest") || strings.HasPrefix(cmd, "mocha") ||
		strings.HasPrefix(cmd, "jasmine"):
		out = compressPytest(lines)
	case strings.HasPrefix(cmd, "tsc") || strings.HasPrefix(cmd, "npx tsc"):
		out = compressTsc(lines)
	default:
		if dedupd := compressLogDedup(lines); dedupd != nil {
			out = dedupd
		} else {
			out = compressGeneric(lines)
		}
	}

	out = collapseBlankLines(out)

	// shorter_only guard: never emit a result that's longer than the original.
	if len(out) >= originalLines {
		return output, originalLines, originalLines
	}

	if len(out) > maxLines {
		cut := len(out) - maxLines
		out = append([]string{fmt.Sprintf("[%d lines omitted]", cut)}, out[cut:]...)
	}

	return strings.Join(out, "\n"), originalLines, len(out)
}

func (s *Server) compressOutput(_ context.Context, _ *sdk.CallToolRequest, in CompressInput) (*sdk.CallToolResult, CompressOutput, error) {
	text, original, outLines := CompressText(in.Output, in.Command, in.MaxLines)
	if in.Output == "" {
		return nil, CompressOutput{Status: "ok", Compressed: ""}, nil
	}
	saved := 0
	if original > 0 {
		saved = (original - outLines) * 100 / original
	}
	return nil, CompressOutput{
		Status:        "ok",
		Compressed:    text,
		OriginalLines: original,
		OutputLines:   outLines,
		SavedPct:      saved,
	}, nil
}

// ── go test ──────────────────────────────────────────────────────────────────

var (
	reGoTestPass    = regexp.MustCompile(`^ok\s+`)
	reGoTestFail    = regexp.MustCompile(`^(FAIL|---\s+FAIL|panic:|\s+Error:)`)
	reGoTestSkip    = regexp.MustCompile(`^---\s+SKIP`)
	reGoTestCovLine = regexp.MustCompile(`^coverage:`)
)

func compressGoTest(lines []string) []string {
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

var reGoBuildInfo = regexp.MustCompile(`^#\s+`)

func compressGoBuild(lines []string) []string {
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
	reGitDiffHunk    = regexp.MustCompile(`^@@`)
	reGitDiffHunkParse = regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@`)
)

func compressGit(lines []string) []string {
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
// File header lines (diff --git, index, ---, +++) are preserved; @@ hunks
// are replaced by numbered change lines; context lines are dropped.
// A summary line is appended: "diff +A/-D lines".
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
	reCargoCompiling = regexp.MustCompile(`^\s*Compiling\s`)
	reCargoChecking  = regexp.MustCompile(`^\s*Checking\s`)
	reCargoDownload  = regexp.MustCompile(`^\s*Downloading|^\s*Downloaded\s`)
)

func compressCargo(lines []string) []string {
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
	reNpmProgress = regexp.MustCompile(`^\s*(npm warn|npm notice|[⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏]|▐|▌|\[[\d.]+s\])`)
	reNpmAdded    = regexp.MustCompile(`added \d+ package`)
	reNpmErr      = regexp.MustCompile(`(?i)^(npm ERR!|error\b|warn\b)`)
)

func compressNpm(lines []string) []string {
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
	reDockerStep   = regexp.MustCompile(`^Step \d+/\d+`)
	reDockerArrow  = regexp.MustCompile(`^ --->`)
	reDockerRemove = regexp.MustCompile(`^Removing intermediate`)
)

func compressDocker(lines []string) []string {
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
	reProgressLine = regexp.MustCompile(`(\d+%|[\d.]+/[\d.]+\s*(MB|KB|GB)|ETA|elapsed|\[=+>?\s*\])`)
	reTimestamp    = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}`)
)

func compressGeneric(lines []string) []string {
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
	reKubectlHealthy  = regexp.MustCompile(`\s+Running\s+0\s+`)
	reKubectlProgress = regexp.MustCompile(`^(Waiting for|waiting for|Watching)`)
	reKubectlBoiler   = regexp.MustCompile(`^(Warning: resource|kubectl\.kubernetes\.io)`)
	reKubectlLogTS    = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}`)
)

func compressKubectl(lines []string) []string {
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
	reMakeEnter   = regexp.MustCompile(`^make(\[\d+\])?: (Entering|Leaving) directory`)
	reMakeNothing = regexp.MustCompile(`^make(\[\d+\])?: Nothing to be done`)
	reMakeTarget  = regexp.MustCompile(`^make(\[\d+\])?:`)
)

func compressMake(lines []string) []string {
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
	reGhSeparator = regexp.MustCompile(`^─+$|^═+$`)
	reGhLabel     = regexp.MustCompile(`^(Labels|Assignees|Projects|Milestone|Reviewers):\s*$`)
)

func compressGh(lines []string) []string {
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
	rePipCollect  = regexp.MustCompile(`^(Collecting|Downloading|Using cached|Obtaining)\s`)
	rePipProgress = regexp.MustCompile(`^\s+\d+%\|`)
	rePipDepSolve = regexp.MustCompile(`^(Requirement already satisfied|Looking in indexes)`)
	rePipResolvUV = regexp.MustCompile(`^\s+(Resolved|Downloaded|Prepared|Installed|Uninstalled)\s`)
)

func compressPip(lines []string) []string {
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
	reTFRefresh   = regexp.MustCompile(`^.+: (Refreshing state\.\.\.|Still (creating|destroying|modifying)\.\.\.)`)
	reTFProgress  = regexp.MustCompile(`^\s*[\d.]+s elapsed`)
	reTFModHeader = regexp.MustCompile(`^Terraform (will|has) (perform|been working)`)
)

func compressTerraform(lines []string) []string {
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
	reCmakeProgress = regexp.MustCompile(`^\[\s*\d+%\]`)
	reCmakeDashes   = regexp.MustCompile(`^-{10,}`)
	reNinjaProgress = regexp.MustCompile(`^\[\d+/\d+\] `)
	reNinjaWarning  = regexp.MustCompile(`warning:`)
)

func compressCmake(lines []string) []string {
	out := make([]string, 0, len(lines))
	warnCount := 0
	for _, l := range lines {
		switch {
		case reCmakeDashes.MatchString(l):
			// drop separator lines
		case reCmakeProgress.MatchString(l):
			// drop "[ 42%] Building..." progress lines
		case reNinjaProgress.MatchString(l):
			// drop ninja "[N/M] Compiling..." lines; keep warnings
			if reNinjaWarning.MatchString(l) {
				warnCount++
				if warnCount <= 5 {
					out = append(out, l)
				}
			}
		default:
			out = append(out, l)
		}
	}
	if warnCount > 5 {
		out = append(out, fmt.Sprintf("[%d additional warnings omitted]", warnCount-5))
	}
	if len(out) == 0 {
		return lines
	}
	return out
}

func collapseBlankLines(lines []string) []string {
	out := make([]string, 0, len(lines))
	blanks := 0
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			blanks++
			if blanks <= 1 {
				out = append(out, l)
			}
		} else {
			blanks = 0
			out = append(out, l)
		}
	}
	return out
}

// compactLines returns at most max non-empty lines with a trailer if truncated.
func compactLines(lines []string, max int) []string {
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

// ── grep / rg / ag ────────────────────────────────────────────────────────────

var reGrepLine = regexp.MustCompile(`^([^:]+):(\d+):(.*)$`)

func compressGrep(lines []string) []string {
	type match struct {
		line    int
		content string
	}
	byFile := make(map[string][]match)
	order := []string{}
	total := 0
	for _, l := range lines {
		caps := reGrepLine.FindStringSubmatch(l)
		if caps == nil {
			continue
		}
		file := caps[1]
		lineNo := 0
		_, _ = fmt.Sscanf(caps[2], "%d", &lineNo)
		content := strings.TrimSpace(caps[3])
		if len(content) > 120 {
			content = content[:119] + "…"
		}
		if _, seen := byFile[file]; !seen {
			order = append(order, file)
		}
		byFile[file] = append(byFile[file], match{lineNo, content})
		total++
	}
	if total == 0 {
		return lines
	}

	perFile := 10
	if total > 200 {
		perFile = 5
	}

	var out []string
	out = append(out, fmt.Sprintf("%d matches in %dF:", total, len(byFile)))
	// sort by most matches first
	sort.Slice(order, func(i, j int) bool {
		return len(byFile[order[i]]) > len(byFile[order[j]])
	})
	for _, file := range order {
		matches := byFile[file]
		path := strings.TrimPrefix(file, "./")
		out = append(out, fmt.Sprintf("\n%s (%d):", path, len(matches)))
		shown := matches
		if len(shown) > perFile {
			shown = shown[:perFile]
		}
		for _, m := range shown {
			if m.line > 0 {
				out = append(out, fmt.Sprintf("  %d: %s", m.line, m.content))
			} else {
				out = append(out, fmt.Sprintf("  %s", m.content))
			}
		}
		if len(matches) > perFile {
			out = append(out, fmt.Sprintf("  ... +%d more", len(matches)-perFile))
		}
	}
	return out
}

// ── find / fd ─────────────────────────────────────────────────────────────────

var reFindSkip = regexp.MustCompile(`node_modules/|\.git/|target/(debug|release)/|__pycache__/|\.next/|dist/`)

func compressFind(lines []string) []string {
	byDir := make(map[string][]string)
	order := []string{}
	total := 0
	for _, l := range lines {
		path := strings.TrimSpace(strings.TrimPrefix(l, "./"))
		if path == "" || reFindSkip.MatchString(path) {
			continue
		}
		total++
		slash := strings.LastIndex(path, "/")
		var dir, file string
		if slash >= 0 {
			dir = path[:slash]
			file = path[slash+1:]
		} else {
			dir = "."
			file = path
		}
		if _, seen := byDir[dir]; !seen {
			order = append(order, dir)
		}
		byDir[dir] = append(byDir[dir], file)
	}
	if total == 0 || len(lines) < 5 {
		return lines
	}

	sort.Strings(order)
	var out []string
	out = append(out, fmt.Sprintf("%dF %dD:", total, len(byDir)))
	for _, dir := range order {
		files := byDir[dir]
		out = append(out, "")
		out = append(out, dir+"/")
		shown := files
		if len(shown) > 10 {
			shown = shown[:10]
		}
		var buf strings.Builder
		for _, f := range shown {
			if buf.Len() > 0 && buf.Len()+len(f)+1 > 60 {
				out = append(out, "  "+buf.String())
				buf.Reset()
			}
			if buf.Len() > 0 {
				buf.WriteByte(' ')
			}
			buf.WriteString(f)
		}
		if buf.Len() > 0 {
			out = append(out, "  "+buf.String())
		}
		if len(files) > 10 {
			out = append(out, fmt.Sprintf("  ... +%d more", len(files)-10))
		}
	}
	return out
}

// ── eslint / biome ────────────────────────────────────────────────────────────

var (
	reEslintFile = regexp.MustCompile(`^(/\S+|[A-Za-z]:\\\S+|\S+\.\w+)$`)
	reEslintDiag = regexp.MustCompile(`^\s+(\d+):(\d+)\s+(error|warning)\s+(.+?)\s{2,}(\S+)\s*$`)
)

func compressEslint(lines []string) []string {
	byRule := make(map[string][2]int) // [errors, warnings]
	ruleOrder := []string{}
	fileCount := 0
	totalErr, totalWarn := 0, 0

	for _, l := range lines {
		if reEslintFile.MatchString(strings.TrimSpace(l)) {
			fileCount++
			continue
		}
		if caps := reEslintDiag.FindStringSubmatch(l); caps != nil {
			rule := caps[5]
			cur := byRule[rule]
			if _, seen := byRule[rule]; !seen {
				ruleOrder = append(ruleOrder, rule)
			}
			if caps[3] == "error" {
				cur[0]++
				totalErr++
			} else {
				cur[1]++
				totalWarn++
			}
			byRule[rule] = cur
		}
	}

	if totalErr == 0 && totalWarn == 0 {
		if len(lines) <= 5 {
			return lines
		}
		return compactLines(lines, 10)
	}

	var out []string
	out = append(out, fmt.Sprintf("%d errors, %d warnings in %d files", totalErr, totalWarn, fileCount))

	sort.Slice(ruleOrder, func(i, j int) bool {
		ai, aj := byRule[ruleOrder[i]], byRule[ruleOrder[j]]
		return ai[0]+ai[1] > aj[0]+aj[1]
	})
	for _, rule := range ruleOrder {
		if len(out)-1 >= 10 {
			break
		}
		cnt := byRule[rule]
		out = append(out, fmt.Sprintf("  %s: %d", rule, cnt[0]+cnt[1]))
	}
	if len(ruleOrder) > 10 {
		out = append(out, fmt.Sprintf("  ... +%d more rules", len(ruleOrder)-10))
	}
	return out
}

// ── ruff ──────────────────────────────────────────────────────────────────────

var (
	reRuffCheck  = regexp.MustCompile(`^(.+?):(\d+):(\d+):\s+([A-Z]\d+)\s+(.+)$`)
	reRuffFixed  = regexp.MustCompile(`Found (\d+) errors?.*?(\d+) fixable`)
	reRuffFormat = regexp.MustCompile(`(\d+) files? reformatted`)
)

func compressRuff(cmd string, lines []string) []string {
	joined := strings.Join(lines, "\n")
	trimmed := strings.TrimSpace(joined)
	if trimmed == "" || strings.Contains(trimmed, "All checks passed") {
		return []string{"clean"}
	}

	if strings.Contains(cmd, "format") || strings.Contains(cmd, "fmt") {
		if caps := reRuffFormat.FindStringSubmatch(joined); caps != nil {
			return []string{caps[0]}
		}
		return compactLines(lines, 5)
	}

	byRule := make(map[string]int)
	ruleOrder := []string{}
	files := make(map[string]struct{})
	var issueLines []string

	for _, l := range lines {
		if caps := reRuffCheck.FindStringSubmatch(l); caps != nil {
			file, rule := caps[1], caps[4]
			files[file] = struct{}{}
			if _, seen := byRule[rule]; !seen {
				ruleOrder = append(ruleOrder, rule)
			}
			byRule[rule]++
			issueLines = append(issueLines, l)
		}
	}

	if len(byRule) == 0 {
		if caps := reRuffFixed.FindStringSubmatch(joined); caps != nil {
			return []string{fmt.Sprintf("%s errors (%s fixable)", caps[1], caps[2])}
		}
		return compactLines(lines, 10)
	}

	total := len(issueLines)
	if total <= 30 {
		return lines
	}

	sort.Slice(ruleOrder, func(i, j int) bool {
		return byRule[ruleOrder[i]] > byRule[ruleOrder[j]]
	})

	var out []string
	out = append(out, fmt.Sprintf("%d issues in %d files", total, len(files)))
	shown := issueLines
	if len(shown) > 20 {
		shown = shown[:20]
	}
	for _, l := range shown {
		out = append(out, "  "+l)
	}
	if len(issueLines) > 20 {
		out = append(out, fmt.Sprintf("  ... +%d more issues", len(issueLines)-20))
	}
	out = append(out, "", "by rule:")
	for i, rule := range ruleOrder {
		if i >= 8 {
			out = append(out, fmt.Sprintf("  ... +%d more rules", len(ruleOrder)-8))
			break
		}
		out = append(out, fmt.Sprintf("  %s: %d", rule, byRule[rule]))
	}
	return out
}

// ── mypy ──────────────────────────────────────────────────────────────────────

var (
	reMypyDiag    = regexp.MustCompile(`^(.+?):(\d+):\s+(error|warning|note):\s+(.+?)(?:\s+\[(.+)\])?$`)
	reMypySummary = regexp.MustCompile(`Found (\d+) errors? in (\d+) files?`)
)

func compressMypy(lines []string) []string {
	byCode := make(map[string]int)
	codeOrder := []string{}
	bySev := make(map[string]int)
	files := make(map[string]struct{})
	var first []string

	for _, l := range lines {
		if caps := reMypyDiag.FindStringSubmatch(l); caps != nil {
			file, sev := caps[1], caps[3]
			code := caps[5]
			files[file] = struct{}{}
			bySev[sev]++
			if code != "" {
				if _, seen := byCode[code]; !seen {
					codeOrder = append(codeOrder, code)
				}
				byCode[code]++
			}
			if len(first) < 5 {
				short := file
				if i := strings.LastIndex(file, "/"); i >= 0 {
					short = file[i+1:]
				}
				codeStr := "?"
				if code != "" {
					codeStr = code
				}
				first = append(first, fmt.Sprintf("  %s:%s [%s] %s", short, caps[2], codeStr, caps[4]))
			}
		}
	}

	joined := strings.Join(lines, "\n")
	if caps := reMypySummary.FindStringSubmatch(joined); caps != nil {
		sort.Slice(codeOrder, func(i, j int) bool {
			return byCode[codeOrder[i]] > byCode[codeOrder[j]]
		})
		var out []string
		out = append(out, fmt.Sprintf("%s errors in %s files", caps[1], caps[2]))
		for i, code := range codeOrder {
			if i >= 6 {
				out = append(out, fmt.Sprintf("  ... +%d more codes", len(codeOrder)-6))
				break
			}
			out = append(out, fmt.Sprintf("  [%s]: %d", code, byCode[code]))
		}
		if len(first) > 0 {
			out = append(out, "Top errors:")
			out = append(out, first...)
		}
		return out
	}

	if len(files) > 0 {
		total := 0
		for _, v := range bySev {
			total += v
		}
		var out []string
		out = append(out, fmt.Sprintf("%d issues in %d files (%d errors, %d warnings)",
			total, len(files), bySev["error"], bySev["warning"]))
		out = append(out, first...)
		return out
	}

	return compactLines(lines, 8)
}

// ── pytest ────────────────────────────────────────────────────────────────────

var (
	rePytestResult  = regexp.MustCompile(`^(PASSED|FAILED|ERROR)\s+(.+?)(?:\s+-\s+(.+))?$`)
	rePytestSummary = regexp.MustCompile(`=+ ([\d]+ passed|[\d]+ failed|[\w ,]+) in ([\d.]+)s =+`)
	rePytestShort   = regexp.MustCompile(`^(FAILED|ERROR) (.+)$`)
)

func compressPytest(lines []string) []string {
	var failed []string
	var summaryLine string

	for _, l := range lines {
		if caps := rePytestSummary.FindStringSubmatch(l); caps != nil {
			summaryLine = l
		}
		if caps := rePytestShort.FindStringSubmatch(l); caps != nil {
			name := strings.TrimSpace(caps[2])
			failed = append(failed, "  FAIL: "+name)
		}
		_ = rePytestResult
	}

	if summaryLine == "" {
		return compactLines(lines, 20)
	}

	var out []string
	// keep the summary bar
	out = append(out, strings.TrimSpace(summaryLine))
	if len(failed) > 0 && len(failed) <= 20 {
		out = append(out, "")
		out = append(out, failed...)
	} else if len(failed) > 20 {
		out = append(out, "")
		out = append(out, failed[:20]...)
		out = append(out, fmt.Sprintf("  ... +%d more failures", len(failed)-20))
	}
	return out
}

// ── tsc ───────────────────────────────────────────────────────────────────────

var (
	reTscError = regexp.MustCompile(`(\S+)\((\d+),\d+\):\s+error\s+(TS\d+):\s+(.+)`)
	reTscCount = regexp.MustCompile(`Found (\d+) error`)
)

func compressTsc(lines []string) []string {
	var errors []string
	files := make(map[string]struct{})
	totalErrors := 0

	joined := strings.Join(lines, "\n")
	if caps := reTscCount.FindStringSubmatch(joined); caps != nil {
		_, _ = fmt.Sscanf(caps[1], "%d", &totalErrors)
	}

	for _, l := range lines {
		if caps := reTscError.FindStringSubmatch(l); caps != nil {
			file, lineNo, code, msg := caps[1], caps[2], caps[3], caps[4]
			files[file] = struct{}{}
			if len(msg) > 40 {
				msg = msg[:40] + "..."
			}
			if len(errors) < 10 {
				short := file
				if i := strings.LastIndex(file, "/"); i >= 0 {
					short = file[i+1:]
				}
				errors = append(errors, fmt.Sprintf("  %s:%s %s %s", short, lineNo, code, msg))
			}
		}
	}

	if len(errors) == 0 {
		return lines
	}

	var out []string
	out = append(out, fmt.Sprintf("%d errors in %d files:", totalErrors, len(files)))
	out = append(out, errors...)
	return out
}

// ── log dedup ─────────────────────────────────────────────────────────────────

var reLogTimestamp = regexp.MustCompile(`^\[?\d{4}[-/]\d{2}[-/]\d{2}[T ]\d{2}:\d{2}:\d{2}[^\]\s]*\]?\s*`)

// compressLogDedup strips timestamps and deduplicates repeated lines.
// Returns nil if it can't reduce the input (fewer than 10 lines or no duplicates found).
func compressLogDedup(lines []string) []string {
	if len(lines) < 10 {
		return nil
	}

	type entry struct {
		line  string
		count int
	}
	var entries []entry
	seen := make(map[string]int) // key → index in entries

	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		key := reLogTimestamp.ReplaceAllString(l, "")
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if idx, ok := seen[key]; ok {
			entries[idx].count++
		} else {
			seen[key] = len(entries)
			entries = append(entries, entry{l, 1})
		}
	}

	// Only apply if we actually removed duplicates.
	if len(entries) >= len(lines) {
		return nil
	}

	var out []string
	for _, e := range entries {
		if e.count > 1 {
			out = append(out, fmt.Sprintf("%s  (x%d)", e.line, e.count))
		} else {
			out = append(out, e.line)
		}
	}
	return out
}
