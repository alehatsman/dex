package mcp

import (
	"context"
	"fmt"
	"regexp"
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

// CompressText applies command-specific and generic compression patterns to
// output text. command is a hint (e.g. "go test", "git diff") that selects
// the pattern set; an empty or unrecognised command falls back to the generic
// pass. maxLines caps the result (0 → 200). This is the pure-text entry point
// shared by the MCP tool and the CLI compress-stdin command.
func CompressText(output, command string, maxLines int) (compressed string, originalLines, outputLines int) {
	if output == "" {
		return "", 0, 0
	}
	if maxLines <= 0 {
		maxLines = 200
	}

	lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
	originalLines = len(lines)

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
		strings.HasPrefix(cmd, "bun ") || strings.HasPrefix(cmd, "pnpm "):
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
		strings.HasPrefix(cmd, "uv "):
		out = compressPip(lines)
	case strings.HasPrefix(cmd, "terraform") || strings.HasPrefix(cmd, "tofu"):
		out = compressTerraform(lines)
	case strings.HasPrefix(cmd, "cmake") || strings.HasPrefix(cmd, "ninja"):
		out = compressCmake(lines)
	default:
		out = compressGeneric(lines)
	}

	out = collapseBlankLines(out)
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
		case strings.HasPrefix(l, "=== RUN"):
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
	reGitDiffContext = regexp.MustCompile(`^[ ]`)
)

func compressGit(lines []string) []string {
	// For long diffs, keep only +/- lines and hunk headers; drop context.
	if len(lines) < 80 {
		return lines
	}
	var out []string
	for _, l := range lines {
		switch {
		case reGitDiffHunk.MatchString(l):
			out = append(out, l) // keep @@ hunk headers
		case reGitDiffContext.MatchString(l):
			// drop unchanged context lines
		default:
			out = append(out, l)
		}
	}
	if len(out) >= len(lines) {
		return lines
	}
	return out
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
