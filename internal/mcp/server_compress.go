package mcp

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/alehatsman/dex/internal/compress"
	"github.com/alehatsman/dex/internal/tokens"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type CompressInput struct {
	Output      string  `json:"output"                 jsonschema:"raw command output to compress"`
	Command     string  `json:"command,omitempty"      jsonschema:"command name hint (e.g. 'go test', 'git log', 'npm install') — selects compression patterns"`
	MaxLines    int     `json:"max_lines,omitempty"    jsonschema:"hard cap on output lines (default 200)"`
	TargetRatio float64 `json:"target_ratio,omitempty" jsonschema:"optional output/input token ratio target in (0,1) — e.g. 0.4 means compress to 40% of original; uses information-bottleneck binary search; applied after pattern passes"`
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
	case (strings.HasPrefix(cmd, "cat ") || strings.HasPrefix(cmd, "bat ") ||
		strings.HasPrefix(cmd, "batcat ")) && isDepsFilename(depsFileArg(cmd)):
		// cat package.json / cat go.mod / cat Cargo.toml → compact deps summary
		if summary, ok := compressDepsFile(depsFileArg(cmd), []byte(strings.Join(lines, "\n"))); ok {
			out = strings.Split(summary, "\n")
		}
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
	case strings.HasPrefix(cmd, "npx playwright") || strings.HasPrefix(cmd, "playwright") ||
		strings.HasPrefix(cmd, "npx cypress") || strings.HasPrefix(cmd, "cypress"):
		out = compressPlaywright(cmd, lines)
	case strings.HasPrefix(cmd, "next build") || strings.HasPrefix(cmd, "npx next build") ||
		strings.HasPrefix(cmd, "vite build") || strings.HasPrefix(cmd, "npx vite build"):
		out = compressNextBuild(cmd, lines)
	case strings.HasPrefix(cmd, "helm "):
		out = compressHelm(cmd, lines)
	case strings.HasPrefix(cmd, "ansible") || strings.HasPrefix(cmd, "ansible-playbook"):
		out = compressAnsible(lines)
	case strings.HasPrefix(cmd, "mvn ") || strings.HasPrefix(cmd, "./mvnw ") ||
		strings.HasPrefix(cmd, "mvnw ") || strings.HasPrefix(cmd, "gradle ") ||
		strings.HasPrefix(cmd, "./gradlew ") || strings.HasPrefix(cmd, "gradlew "):
		out = compressMaven(cmd, lines)
	case strings.HasPrefix(cmd, "bazel "):
		out = compressBazel(cmd, lines)
	case strings.HasPrefix(cmd, "poetry "):
		out = compressPoetry(cmd, lines)
	case strings.HasPrefix(cmd, "npx prisma ") || strings.HasPrefix(cmd, "prisma "):
		out = compressPrisma(cmd, lines)
	case strings.HasPrefix(cmd, "prettier ") || strings.HasPrefix(cmd, "npx prettier "):
		out = compressPrettier(lines)
	case strings.HasPrefix(cmd, "rubocop") || strings.HasPrefix(cmd, "bundle ") ||
		strings.HasPrefix(cmd, "rake ") || strings.HasPrefix(cmd, "rails "):
		out = compressRuby(cmd, lines)
	case strings.HasPrefix(cmd, "composer "):
		out = compressComposer(cmd, lines)
	case strings.HasPrefix(cmd, "php artisan "):
		out = compressArtisan(cmd[4:], lines)
	case strings.HasPrefix(cmd, "mix "):
		out = compressMix(cmd, lines)
	case strings.HasPrefix(cmd, "swift "):
		out = compressSwiftBuild(cmd, lines)
	case strings.HasPrefix(cmd, "zig "):
		out = compressZig(cmd, lines)
	case strings.HasPrefix(cmd, "ps ") || cmd == "ps":
		if compressed := compressPs(lines); compressed != nil {
			out = compressed
		}
	case strings.HasPrefix(cmd, "du ") || cmd == "du":
		if compressed := compressDu(lines); compressed != nil {
			out = compressed
		}
	case strings.HasPrefix(cmd, "ping "):
		if compressed := compressPing(lines); compressed != nil {
			out = compressed
		}
	case strings.HasPrefix(cmd, "systemctl ") || cmd == "systemctl" ||
		strings.HasPrefix(cmd, "journalctl"):
		out = compressSystemd(cmd, lines)
	case cmd == "ls" || strings.HasPrefix(cmd, "ls ") || strings.HasPrefix(cmd, "ls -"):
		if compressed := compressLs(lines); compressed != nil {
			out = compressed
		}
	case strings.HasPrefix(cmd, "mysql ") || cmd == "mysql" ||
		strings.HasPrefix(cmd, "mariadb "):
		out = compressMySQL(cmd, lines)
	case strings.HasPrefix(cmd, "psql ") || cmd == "psql":
		out = compressPsql(cmd, lines)
	case strings.HasPrefix(cmd, "env") || cmd == "env" || cmd == "printenv" ||
		strings.HasPrefix(cmd, "printenv ") || cmd == "export" ||
		strings.HasPrefix(cmd, "export "):
		out = compressEnvFilter(lines)
	default:
		if blocked := compressLogBlock(lines); blocked != nil {
			out = blocked
		} else if dedupd := compressLogDedup(lines); dedupd != nil {
			out = dedupd
		} else {
			out = compressGeneric(lines)
		}
	}

	out = collapseBlankLines(out)

	// Entropy pass: drop low-information lines using Shannon entropy + marker
	// + trigram-repetition scoring. Quality gate preserves paths and idents.
	if ef := compress.EntropyFilter(out, compress.EntropyThresholdStandard); ef != nil {
		out = ef
	}

	// Terse pass: deterministic function-word stripping + abbreviations +
	// zero-unique-token line dedup. Quality gate (3% minimum) is internal.
	if tr := compress.TerseCompress(strings.Join(out, "\n"), compress.Level3); tr.Applied {
		out = strings.Split(tr.Output, "\n")
	}

	// shorter_only guard: never emit a result that's longer than the original.
	if len(out) >= originalLines {
		return output, originalLines, originalLines
	}

	// Over-compression guard: >95% token reduction on small output is almost
	// always signal loss (e.g. compressing a one-line compiler error to nothing).
	origTok := estimateTokens(stripped)
	if origTok > 100 && origTok < 2000 {
		if float64(estimateTokens(strings.Join(out, "\n")))/float64(origTok) < 0.05 {
			return output, originalLines, originalLines
		}
	}

	if len(out) > maxLines {
		cut := len(out) - maxLines
		omitted := out[:cut]
		tail := out[cut:]
		needles := extractSafetyLines(omitted, 200)
		if len(needles) > 0 {
			header := fmt.Sprintf("[%d lines omitted, %d diagnostic lines preserved]", cut, len(needles))
			var head []string
			head = append(head, header)
			head = append(head, needles...)
			out = append(head, tail...)
		} else {
			notice := fmt.Sprintf("[%d lines omitted — output too large for context window]", cut)
			out = append([]string{notice}, tail...)
		}
	}

	return strings.Join(out, "\n"), originalLines, len(out)
}

// estimateTokens returns a real BPE token count (default o200k_base) so the
// over-compression guard's ratio test triggers on the tokens the model
// actually sees rather than a whitespace-word approximation.
func estimateTokens(s string) int { return tokens.Count(s) }

// safetyNeedles are patterns that must survive truncation — errors, panics,
// test outcomes, security events, and diagnostic markers.
var safetyNeedleStrs = []string{
	"CRITICAL", "FATAL", "panic", "FAILED", "unhealthy", "Exited", "OOMKilled",
	"DETACHED HEAD", "detached", "vulnerability", "CVE-", "denied", "unauthorized",
	"forbidden", "segfault", "Segmentation fault", "SIGSEGV", "SIGKILL", "killed",
	"out of memory", "stack overflow", "permission denied", "certificate", "expired",
	"corrupt", "test result:", "panicked", "assertion", "traceback", "tests run",
	// lower-case variants matched case-insensitively via strings.Contains on lower
	"error", "warning", "failed", "passed", "passing",
}

// extractSafetyLines scans lines for safety needles and returns up to max
// matching lines (preserving order). Case-insensitive match.
func extractSafetyLines(lines []string, max int) []string {
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

func (s *Server) compressOutput(_ context.Context, _ *sdk.CallToolRequest, in CompressInput) (*sdk.CallToolResult, CompressOutput, error) {
	if in.Output == "" {
		return nil, CompressOutput{Status: "ok", Compressed: ""}, nil
	}
	text, original, outLines := CompressText(in.Output, in.Command, in.MaxLines)

	// Information-bottleneck pass: binary-search entropy threshold to hit the
	// caller's target ratio. Applied after pattern passes so the IB search
	// operates on already-compressed output.
	if in.TargetRatio > 0 && in.TargetRatio < 1 {
		if ib := compress.CompressIB(text, in.TargetRatio); ib != text {
			text = ib
			outLines = len(strings.Split(text, "\n"))
		}
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

// ── verbatim compact ─────────────────────────────────────────────────────────

var (
	// Matches ISO 8601, space-separated datetime, and common log timestamp formats.
	reTS = regexp.MustCompile(
		`\d{4}[-/]\d{2}[-/]\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?`)
	// Matches 32-64 char lowercase hex strings (commit hashes, checksums, IDs).
	reHex = regexp.MustCompile(`\b[0-9a-f]{32,64}\b`)
	// Matches RFC 4122 UUIDs (xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx).
	reUUID = regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`)
	// Matches common log-level prefixes at the start of a line or after a timestamp.
	reLogLevel = regexp.MustCompile(`\b(DEBUG|INFO|WARN(?:ING)?|ERROR|CRITICAL|FATAL|TRACE)\b\s*:?\s*`)
	// Separator lines (====, ----, ***) that occupy >80% of non-space characters.
	reSep = regexp.MustCompile(`^[\s=\-*#]{3,}$`)
)

// normalizeTimestamps replaces ISO 8601 / datetime strings in a line with [TS].
func normalizeTimestamps(s string) string { return reTS.ReplaceAllString(s, "[TS]") }

// normalizeHashes replaces 32–64 char hex strings with [HASH].
func normalizeHashes(s string) string { return reHex.ReplaceAllString(s, "[HASH]") }

// normalizeUUIDs replaces RFC 4122 UUIDs with [UUID] for dedup key computation.
func normalizeUUIDs(s string) string { return reUUID.ReplaceAllString(s, "[UUID]") }

// normalizeLineForDedup returns a key for dedup comparison: timestamps, hashes,
// UUIDs, and log-level labels are replaced so lines that differ only in those
// fields are treated as duplicates.
func normalizeLineForDedup(s string) string {
	s = normalizeTimestamps(s)
	s = normalizeHashes(s)
	s = normalizeUUIDs(s)
	s = reLogLevel.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

// isBoilerplateLine returns true for copyright notices, license headers,
// code-gen banners, and decorative separator lines.
func isBoilerplateLine(s string) bool {
	t := strings.TrimSpace(s)
	if t == "" {
		return false
	}
	lower := strings.ToLower(t)
	for _, kw := range []string{
		"copyright", "licensed under", "all rights reserved",
		"generated by", "do not edit", "auto-generated",
	} {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	// Separator lines: nearly all non-space chars are =, -, *, #
	if reSep.MatchString(t) && len(strings.TrimLeft(t, "=- *#\t")) == 0 {
		return true
	}
	return false
}

// verbatimCompact applies lean-ctx's verbatim_compact pipeline:
//  1. Strip boilerplate lines (copyright, separator, code-gen banners).
//  2. Normalize timestamps → [TS] and hashes → [HASH] in the output.
//  3. Deduplicate consecutive near-identical lines (same after normalization),
//     emitting "[Nx] line" for runs of N>1.
//  4. Collapse runs of blank lines to at most one.
//
// Returns nil when no reduction was achieved.
func verbatimCompact(lines []string) []string {
	if len(lines) < 10 {
		return nil
	}
	var out []string
	var prevNorm string
	var prevDisplay string
	var run int
	blankRun := 0

	flush := func() {
		if prevDisplay == "" {
			return
		}
		if run > 1 {
			out = append(out, fmt.Sprintf("[%dx] %s", run, prevDisplay))
		} else {
			out = append(out, prevDisplay)
		}
		prevDisplay = ""
		prevNorm = ""
		run = 0
	}

	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			flush()
			blankRun++
			if blankRun == 1 {
				out = append(out, "")
			}
			continue
		}
		blankRun = 0

		if isBoilerplateLine(l) {
			continue
		}

		display := normalizeTimestamps(l)
		display = normalizeHashes(display)
		norm := normalizeLineForDedup(l)

		if norm == prevNorm && norm != "" {
			run++
		} else {
			flush()
			prevDisplay = display
			prevNorm = norm
			run = 1
		}
	}
	flush()

	if len(out) >= len(lines) {
		return nil
	}
	return out
}

// compressLogDedup is a lighter variant of verbatimCompact: it only deduplicates
// using the timestamp-stripped key without touching the displayed text.
// Used as a pre-check; verbatimCompact is the full replacement.
func compressLogDedup(lines []string) []string {
	compact := verbatimCompact(lines)
	return compact
}

// ── log block compressor ──────────────────────────────────────────────────────

var (
	reBlockHeader  = regexp.MustCompile(`^(===|---|###|##|Step\s+\d|STEP\s+\d|stage\s+\d)`)
	reGitCommitHdr = regexp.MustCompile(`^commit\s+[0-9a-f]{7,}`)
	reGitDiffHdrB  = regexp.MustCompile(`^diff --git `)
)

// isBlockBoundary returns true for lines that start a new logical block
// (CI step header, git commit, section separator, markdown h2/h3).
func isBlockBoundary(l string) bool {
	t := strings.TrimSpace(l)
	if t == "" {
		return false
	}
	return reBlockHeader.MatchString(t) ||
		reGitCommitHdr.MatchString(t) ||
		reGitDiffHdrB.MatchString(t)
}

// isErrorLogLine returns true when a line contains a severe-severity keyword.
func isErrorLogLine(l string) bool {
	ll := strings.ToLower(l)
	for _, kw := range []string{"error", "critical", "fatal", "panic", "exception"} {
		if strings.Contains(ll, kw) {
			return true
		}
	}
	return false
}

// dedupBlockLines collapses consecutive near-identical lines (after
// timestamp/hash normalization) into "[Nx] line" entries.
func dedupBlockLines(lines []string) []string {
	var out []string
	var prevNorm, prevDisplay string
	var run int

	flush := func() {
		if prevDisplay == "" {
			return
		}
		if run > 1 {
			out = append(out, fmt.Sprintf("[%dx] %s", run, prevDisplay))
		} else {
			out = append(out, prevDisplay)
		}
	}

	for _, l := range lines {
		display := normalizeTimestamps(l)
		display = normalizeHashes(display)
		norm := normalizeLineForDedup(l)
		if norm == "" {
			continue
		}
		if norm == prevNorm {
			run++
		} else {
			flush()
			prevDisplay = display
			prevNorm = norm
			run = 1
		}
	}
	flush()
	return out
}

// compressLogBlock is a block-aware log compressor for CI/CD and service logs:
//  1. Detects block boundaries (CI step headers, git commits, section marks).
//  2. Deduplicates consecutive lines within each block independently.
//  3. Surfaces error lines at the top of the output.
//  4. Truncates overlong blocks: single block >30 → last 15; multi-block >20 → first 5 + omit + last 5.
//
// Returns nil when the output has no block structure or no significant reduction.
func compressLogBlock(lines []string) []string {
	if len(lines) < 20 {
		return nil
	}

	// Quick scan: count block headers and duplicate lines
	var boundaries int
	var dupes int
	prevNorm := ""
	for _, l := range lines {
		if isBlockBoundary(l) {
			boundaries++
		}
		norm := normalizeLineForDedup(l)
		if norm == prevNorm && norm != "" {
			dupes++
		}
		prevNorm = norm
	}
	// Apply only when there are blocks OR ≥30% duplicate lines
	if boundaries == 0 && float64(dupes)/float64(len(lines)) < 0.30 {
		return nil
	}

	// Collect error lines for surfacing
	var errLines []string
	for _, l := range lines {
		if isErrorLogLine(l) && !isBoilerplateLine(l) {
			errLines = append(errLines, "  "+strings.TrimSpace(l))
		}
	}

	// Split into blocks (each boundary line starts a new block)
	type block struct {
		header string
		body   []string
	}
	var blocks []block
	cur := block{}
	for _, l := range lines {
		if isBlockBoundary(l) {
			if cur.header != "" || len(cur.body) > 0 {
				blocks = append(blocks, cur)
			}
			cur = block{header: l}
		} else {
			cur.body = append(cur.body, l)
		}
	}
	if cur.header != "" || len(cur.body) > 0 {
		blocks = append(blocks, cur)
	}

	const singleBlockMax = 30
	const multiBlockMax = 20
	const headLines = 5
	const tailLines = 5
	const tailLinesLong = 15

	// Build output
	var out []string
	totalUnique := 0

	for _, b := range blocks {
		deduped := dedupBlockLines(b.body)

		var blockOut []string
		if b.header != "" {
			blockOut = append(blockOut, b.header)
		}

		if boundaries > 1 && len(deduped) > multiBlockMax {
			// Multi-block: head + omit notice + tail
			head := deduped[:headLines]
			tail := deduped[len(deduped)-tailLines:]
			omit := len(deduped) - headLines - tailLines
			blockOut = append(blockOut, head...)
			blockOut = append(blockOut, fmt.Sprintf("[%d lines omitted]", omit))
			blockOut = append(blockOut, tail...)
		} else if boundaries <= 1 && len(deduped) > singleBlockMax {
			// Single long block: keep last tailLinesLong
			omit := len(deduped) - tailLinesLong
			blockOut = append(blockOut, fmt.Sprintf("[%d lines omitted]", omit))
			blockOut = append(blockOut, deduped[len(deduped)-tailLinesLong:]...)
		} else {
			blockOut = append(blockOut, deduped...)
		}

		out = append(out, blockOut...)
		out = append(out, "")
		totalUnique += len(deduped)
	}

	if totalUnique >= len(lines) {
		return nil
	}

	// Build header + error summary
	header := fmt.Sprintf("%d lines → %d unique", len(lines), totalUnique)
	var prefix []string
	prefix = append(prefix, header)
	if len(errLines) > 0 {
		prefix = append(prefix, fmt.Sprintf("%d errors:", len(errLines)))
		prefix = append(prefix, errLines...)
	}
	prefix = append(prefix, "")
	return append(prefix, out...)
}

// ── playwright / cypress ──────────────────────────────────────────────────────

var rePwFailed = regexp.MustCompile(`^\s+\d+\)\s+(.+)$`)

// compressPlaywright compresses Playwright and Cypress test output into a
// summary line + failed test list.
func compressPlaywright(command string, lines []string) []string {
	if strings.Contains(command, "cypress") {
		return compressCypress(lines)
	}

	var passed, failed, skipped int
	var failedNames []string
	var duration string

	for _, l := range lines {
		ll := strings.ToLower(strings.TrimSpace(l))
		if n, ok := extractNumberBefore(ll, "passed"); ok {
			passed = n
		}
		if n, ok := extractNumberBefore(ll, "failed"); ok {
			failed = n
		}
		if n, ok := extractNumberBefore(ll, "skipped"); ok {
			skipped = n
		}
		if m := rePwFailed.FindStringSubmatch(l); m != nil {
			failedNames = append(failedNames, strings.TrimSpace(m[1]))
		}
		if strings.Contains(ll, "finished in") || strings.Contains(ll, "duration") {
			duration = strings.TrimSpace(l)
		}
	}

	total := passed + failed + skipped
	if total == 0 {
		return compressGeneric(lines)
	}

	out := []string{
		fmt.Sprintf("%d tests: %d passed, %d failed, %d skipped", total, passed, failed, skipped),
	}
	if len(failedNames) > 0 {
		out = append(out, "failed:")
		for _, n := range failedNames {
			if len(out) > 12 {
				out = append(out, fmt.Sprintf("  ... +%d more", len(failedNames)-10))
				break
			}
			out = append(out, "  "+n)
		}
	}
	if duration != "" {
		out = append(out, duration)
	}
	return out
}

func compressCypress(lines []string) []string {
	var passed, failed, pending int
	for _, l := range lines {
		ll := strings.ToLower(strings.TrimSpace(l))
		if strings.Contains(ll, "passing") {
			passed += extractFirstInt(ll)
		}
		if strings.Contains(ll, "failing") {
			failed += extractFirstInt(ll)
		}
		if strings.Contains(ll, "pending") {
			pending += extractFirstInt(ll)
		}
	}
	total := passed + failed + pending
	if total == 0 {
		return compressGeneric(lines)
	}
	return []string{
		fmt.Sprintf("%d tests: %d passed, %d failed, %d pending", total, passed, failed, pending),
	}
}

// extractNumberBefore finds a number immediately before keyword in s (e.g. "5 passed").
func extractNumberBefore(s, keyword string) (int, bool) {
	pos := strings.Index(s, keyword)
	if pos < 0 {
		return 0, false
	}
	fields := strings.Fields(s[:pos])
	if len(fields) == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(fields[len(fields)-1])
	if err != nil {
		return 0, false
	}
	return n, true
}

// extractFirstInt returns the first integer found in s, or 0.
func extractFirstInt(s string) int {
	for _, f := range strings.Fields(s) {
		if n, err := strconv.Atoi(f); err == nil {
			return n
		}
	}
	return 0
}

// ── next build / vite build ───────────────────────────────────────────────────

var (
	reNextRoute     = regexp.MustCompile(`[○●λƒ◐]\s+(/\S*)`)
	reNextSize      = regexp.MustCompile(`(\d+\.?\d*)\s*(kB|MB|B)\b`)
	reNextBuildTime = regexp.MustCompile(`(?:compiled|built|done|ready)\s+(?:in\s+)?(\d+\.?\d*\s*[ms]+)`)
	reViteChunk     = regexp.MustCompile(`dist/(\S+)\s+(\d+\.?\d*\s*[kKMm]?B)`)
)

// compressNextBuild compresses Next.js and Vite build output into a route/chunk
// summary with total size and build time.
func compressNextBuild(command string, lines []string) []string {
	if strings.Contains(command, "vite") {
		return compressViteBuild(lines)
	}

	var routes []string
	var totalKB float64
	var buildTime string
	var errors []string

	for _, l := range lines {
		if m := reNextRoute.FindStringSubmatch(l); m != nil {
			size := ""
			if sm := reNextSize.FindStringSubmatch(l); sm != nil {
				size = sm[1] + " " + sm[2]
			}
			if size != "" {
				routes = append(routes, fmt.Sprintf("%s (%s)", m[1], size))
			} else {
				routes = append(routes, m[1])
			}
		}
		if m := reNextBuildTime.FindStringSubmatch(strings.ToLower(l)); m != nil {
			buildTime = m[1]
		}
		if ll := strings.ToLower(l); strings.Contains(ll, "error") && !strings.Contains(ll, "0 error") {
			errors = append(errors, strings.TrimSpace(l))
		}
		if sm := reNextSize.FindStringSubmatch(l); sm != nil {
			val, _ := strconv.ParseFloat(sm[1], 64)
			switch sm[2] {
			case "MB":
				totalKB += val * 1024
			case "kB":
				totalKB += val
			default:
				totalKB += val / 1024
			}
		}
	}

	if len(errors) > 0 {
		out := []string{"BUILD ERROR:"}
		return append(out, errors...)
	}

	header := "built"
	if buildTime != "" {
		header = fmt.Sprintf("built (%s)", buildTime)
	}
	out := []string{header}

	if len(routes) > 0 {
		out = append(out, fmt.Sprintf("%d routes:", len(routes)))
		for _, r := range routes {
			if len(out) > 17 {
				out = append(out, fmt.Sprintf("  ... +%d more", len(routes)-15))
				break
			}
			out = append(out, "  "+r)
		}
	}

	if totalKB > 0 {
		if totalKB > 1024 {
			out = append(out, fmt.Sprintf("total: %.1f MB", totalKB/1024))
		} else {
			out = append(out, fmt.Sprintf("total: %.0f kB", totalKB))
		}
	}

	if len(out) == 1 && out[0] == "built" {
		return compressGeneric(lines)
	}
	return out
}

func compressViteBuild(lines []string) []string {
	var chunks []string
	var buildTime string

	for _, l := range lines {
		if m := reViteChunk.FindStringSubmatch(l); m != nil {
			chunks = append(chunks, fmt.Sprintf("%s: %s", m[1], m[2]))
		}
		if m := reNextBuildTime.FindStringSubmatch(strings.ToLower(l)); m != nil {
			buildTime = m[1]
		}
	}

	header := "built"
	if buildTime != "" {
		header = fmt.Sprintf("built (%s)", buildTime)
	}
	out := []string{header}

	if len(chunks) > 0 {
		out = append(out, fmt.Sprintf("%d chunks:", len(chunks)))
		for _, c := range chunks {
			if len(out) > 12 {
				out = append(out, fmt.Sprintf("  ... +%d more", len(chunks)-10))
				break
			}
			out = append(out, "  "+c)
		}
	}

	if len(out) == 1 && out[0] == "built" {
		return compressGeneric(lines)
	}
	return out
}
