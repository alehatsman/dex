package mcp

import (
	"fmt"
	"strings"

	"github.com/alehatsman/dex/internal/compress"
)

// minCompressLines is the minimum number of lines required before any pattern
// runs — tiny outputs gain nothing and can only be made worse.
const minCompressLines = 5

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

	// Strip ANSI escape codes so patterns match colored terminal output.
	stripped := stripANSI(output)

	lines := strings.Split(strings.TrimRight(stripped, "\n"), "\n")
	originalLines = len(lines)

	// Uniform secret redaction (#461): scrub credential values at this single
	// chokepoint so the policy is identical for every command type — not just
	// `env`. Runs before the early-returns below so tiny outputs and auth-flow
	// device-code/token text are redacted too. Behavior-preserving for
	// non-secret lines (same count/order, byte-identical content).
	lines = compress.RedactSecrets(lines)

	// Skip compression for tiny outputs and auth flows.
	if originalLines < minCompressLines || containsAuthFlow(stripped) {
		return strings.Join(lines, "\n"), originalLines, originalLines
	}

	cmd := strings.ToLower(strings.TrimSpace(command))
	out := dispatchCompressor(cmd, lines)
	out = compress.CollapseBlankLines(out)

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
		return strings.Join(lines, "\n"), originalLines, originalLines
	}

	// Over-compression guard: >95% token reduction on small output is almost
	// always signal loss (e.g. compressing a one-line compiler error to nothing).
	origTok := compress.EstimateTokens(stripped)
	if origTok > 100 && origTok < 2000 {
		if float64(compress.EstimateTokens(strings.Join(out, "\n")))/float64(origTok) < 0.05 {
			return strings.Join(lines, "\n"), originalLines, originalLines
		}
	}

	if len(out) > maxLines {
		cut := len(out) - maxLines
		omitted := out[:cut]
		tail := out[cut:]
		needles := compress.ExtractSafetyLines(omitted, 200)
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

// dispatchCompressor routes cmd to the appropriate category dispatcher and
// returns the first non-nil result, falling back to generic log patterns.
func dispatchCompressor(cmd string, lines []string) []string {
	if out := dispatchBuildTools(cmd, lines); out != nil {
		return out
	}
	if out := dispatchInfra(cmd, lines); out != nil {
		return out
	}
	if out := dispatchJSTools(cmd, lines); out != nil {
		return out
	}
	if out := dispatchPythonTools(cmd, lines); out != nil {
		return out
	}
	if out := dispatchPkgTools(cmd, lines); out != nil {
		return out
	}
	if out := dispatchSystemTools(cmd, lines); out != nil {
		return out
	}
	if out := dispatchMiscTools(cmd, lines); out != nil {
		return out
	}
	return dispatchDefault(lines)
}

func dispatchBuildTools(cmd string, lines []string) []string {
	switch {
	case strings.HasPrefix(cmd, "go test"):
		return compress.CompressGoTest(lines)
	case strings.HasPrefix(cmd, "go build") || strings.HasPrefix(cmd, "go vet"):
		return compress.CompressGoBuild(lines)
	case strings.HasPrefix(cmd, "cargo"):
		return compress.CompressCargo(lines)
	case strings.HasPrefix(cmd, "cmake") || strings.HasPrefix(cmd, "ninja") ||
		strings.HasPrefix(cmd, "gcc ") || strings.HasPrefix(cmd, "g++ ") ||
		strings.HasPrefix(cmd, "cc "):
		return compress.CompressCmake(lines)
	case strings.HasPrefix(cmd, "bazel "):
		return compress.CompressBazel(cmd, lines)
	case strings.HasPrefix(cmd, "mvn ") || strings.HasPrefix(cmd, "./mvnw ") ||
		strings.HasPrefix(cmd, "mvnw ") || strings.HasPrefix(cmd, "gradle ") ||
		strings.HasPrefix(cmd, "./gradlew ") || strings.HasPrefix(cmd, "gradlew "):
		return compress.CompressMaven(cmd, lines)
	case strings.HasPrefix(cmd, "swift "):
		return compress.CompressSwiftBuild(cmd, lines)
	case strings.HasPrefix(cmd, "zig "):
		return compress.CompressZig(cmd, lines)
	}
	return nil
}

func dispatchInfra(cmd string, lines []string) []string {
	switch {
	case strings.HasPrefix(cmd, "git"):
		return compress.CompressGit(lines)
	case strings.HasPrefix(cmd, "gh "):
		return compress.CompressGh(lines)
	case strings.HasPrefix(cmd, "docker"):
		return compress.CompressDocker(lines)
	case strings.HasPrefix(cmd, "kubectl"):
		return compress.CompressKubectl(lines)
	case strings.HasPrefix(cmd, "terraform") || strings.HasPrefix(cmd, "tofu"):
		return compress.CompressTerraform(lines)
	case strings.HasPrefix(cmd, "helm "):
		return compress.CompressHelm(cmd, lines)
	case strings.HasPrefix(cmd, "ansible") || strings.HasPrefix(cmd, "ansible-playbook"):
		return compress.CompressAnsible(lines)
	case strings.HasPrefix(cmd, "make") || strings.HasPrefix(cmd, "gmake"):
		return compress.CompressMake(lines)
	}
	return nil
}

func dispatchJSTools(cmd string, lines []string) []string {
	switch {
	case strings.HasPrefix(cmd, "npm ") || strings.HasPrefix(cmd, "yarn ") ||
		strings.HasPrefix(cmd, "bun ") || strings.HasPrefix(cmd, "pnpm ") ||
		strings.HasPrefix(cmd, "turbo ") || strings.HasPrefix(cmd, "nx "):
		return compress.CompressNpm(lines)
	case strings.HasPrefix(cmd, "eslint") || strings.HasPrefix(cmd, "npx eslint") ||
		strings.HasPrefix(cmd, "biome") || strings.HasPrefix(cmd, "hadolint") ||
		strings.HasPrefix(cmd, "yamllint") || strings.HasPrefix(cmd, "markdownlint") ||
		strings.HasPrefix(cmd, "oxlint"):
		return compress.CompressEslint(lines)
	case strings.HasPrefix(cmd, "tsc") || strings.HasPrefix(cmd, "npx tsc"):
		return compress.CompressTsc(lines)
	case strings.HasPrefix(cmd, "npx playwright") || strings.HasPrefix(cmd, "playwright") ||
		strings.HasPrefix(cmd, "npx cypress") || strings.HasPrefix(cmd, "cypress"):
		return compress.CompressPlaywright(cmd, lines)
	case strings.HasPrefix(cmd, "next build") || strings.HasPrefix(cmd, "npx next build") ||
		strings.HasPrefix(cmd, "vite build") || strings.HasPrefix(cmd, "npx vite build"):
		return compress.CompressNextBuild(cmd, lines)
	case strings.HasPrefix(cmd, "prettier ") || strings.HasPrefix(cmd, "npx prettier "):
		return compress.CompressPrettier(lines)
	case strings.HasPrefix(cmd, "npx prisma ") || strings.HasPrefix(cmd, "prisma "):
		return compress.CompressPrisma(cmd, lines)
	}
	return nil
}

func dispatchPythonTools(cmd string, lines []string) []string {
	switch {
	case strings.HasPrefix(cmd, "pip ") || strings.HasPrefix(cmd, "pip3 ") ||
		strings.HasPrefix(cmd, "uv ") || strings.HasPrefix(cmd, "conda ") ||
		strings.HasPrefix(cmd, "mamba ") || strings.HasPrefix(cmd, "pipx "):
		return compress.CompressPip(lines)
	case strings.HasPrefix(cmd, "ruff"):
		return compress.CompressRuff(cmd, lines)
	case strings.HasPrefix(cmd, "mypy") ||
		strings.HasPrefix(cmd, "pyright") || strings.HasPrefix(cmd, "basedpyright"):
		return compress.CompressMypy(lines)
	case strings.HasPrefix(cmd, "pytest") || strings.HasPrefix(cmd, "python -m pytest") ||
		strings.HasPrefix(cmd, "python3 -m pytest") || strings.HasPrefix(cmd, "vitest") ||
		strings.HasPrefix(cmd, "jest") || strings.HasPrefix(cmd, "mocha") ||
		strings.HasPrefix(cmd, "jasmine"):
		return compress.CompressPytest(lines)
	}
	return nil
}

func dispatchPkgTools(cmd string, lines []string) []string {
	switch {
	case strings.HasPrefix(cmd, "poetry "):
		return compress.CompressPoetry(cmd, lines)
	case strings.HasPrefix(cmd, "rubocop") || strings.HasPrefix(cmd, "bundle ") ||
		strings.HasPrefix(cmd, "rake ") || strings.HasPrefix(cmd, "rails "):
		return compress.CompressRuby(cmd, lines)
	case strings.HasPrefix(cmd, "composer "):
		return compress.CompressComposer(cmd, lines)
	case strings.HasPrefix(cmd, "php artisan "):
		return compress.CompressArtisan(cmd[4:], lines)
	case strings.HasPrefix(cmd, "mix "):
		return compress.CompressMix(cmd, lines)
	}
	return nil
}

func dispatchSystemTools(cmd string, lines []string) []string {
	switch {
	case strings.HasPrefix(cmd, "grep ") || strings.HasPrefix(cmd, "rg ") ||
		strings.HasPrefix(cmd, "ag ") || strings.HasPrefix(cmd, "ack "):
		return compress.CompressGrep(lines)
	case strings.HasPrefix(cmd, "find ") || strings.HasPrefix(cmd, "fd "):
		return compress.CompressFind(lines)
	case strings.HasPrefix(cmd, "ps ") || cmd == "ps":
		return compress.CompressPs(lines)
	case strings.HasPrefix(cmd, "du ") || cmd == "du":
		return compress.CompressDu(lines)
	case strings.HasPrefix(cmd, "ping "):
		return compress.CompressPing(lines)
	case strings.HasPrefix(cmd, "systemctl ") || cmd == "systemctl" ||
		strings.HasPrefix(cmd, "journalctl"):
		return compress.CompressSystemd(cmd, lines)
	}
	return nil
}

func dispatchMiscTools(cmd string, lines []string) []string {
	switch {
	case (strings.HasPrefix(cmd, "cat ") || strings.HasPrefix(cmd, "bat ") ||
		strings.HasPrefix(cmd, "batcat ")) && compress.IsDepsFilename(compress.DepsFileArg(cmd)):
		if summary, ok := compress.CompressDepsFile(compress.DepsFileArg(cmd), []byte(strings.Join(lines, "\n"))); ok {
			return strings.Split(summary, "\n")
		}
		return nil
	case cmd == "ls" || strings.HasPrefix(cmd, "ls ") || strings.HasPrefix(cmd, "ls -"):
		return compress.CompressLs(lines)
	case strings.HasPrefix(cmd, "mysql ") || cmd == "mysql" ||
		strings.HasPrefix(cmd, "mariadb "):
		return compress.CompressMySQL(cmd, lines)
	case strings.HasPrefix(cmd, "psql ") || cmd == "psql":
		return compress.CompressPsql(cmd, lines)
	case strings.HasPrefix(cmd, "env") || cmd == "env" || cmd == "printenv" ||
		strings.HasPrefix(cmd, "printenv ") || cmd == "export" ||
		strings.HasPrefix(cmd, "export "):
		return compress.CompressEnvFilter(lines)
	}
	return nil
}

func dispatchDefault(lines []string) []string {
	if blocked := compress.CompressLogBlock(lines); blocked != nil {
		return blocked
	}
	if dedupd := compress.CompressLogDedup(lines); dedupd != nil {
		return dedupd
	}
	return compress.CompressGeneric(lines)
}
