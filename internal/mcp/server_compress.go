package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/alehatsman/dex/internal/compress"
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
		strings.HasPrefix(cmd, "batcat ")) && compress.IsDepsFilename(compress.DepsFileArg(cmd)):
		// cat package.json / cat go.mod / cat Cargo.toml → compact deps summary
		if summary, ok := compress.CompressDepsFile(compress.DepsFileArg(cmd), []byte(strings.Join(lines, "\n"))); ok {
			out = strings.Split(summary, "\n")
		}
	case strings.HasPrefix(cmd, "go test"):
		out = compress.CompressGoTest(lines)
	case strings.HasPrefix(cmd, "go build") || strings.HasPrefix(cmd, "go vet"):
		out = compress.CompressGoBuild(lines)
	case strings.HasPrefix(cmd, "git"):
		out = compress.CompressGit(lines)
	case strings.HasPrefix(cmd, "cargo"):
		out = compress.CompressCargo(lines)
	case strings.HasPrefix(cmd, "npm ") || strings.HasPrefix(cmd, "yarn ") ||
		strings.HasPrefix(cmd, "bun ") || strings.HasPrefix(cmd, "pnpm ") ||
		strings.HasPrefix(cmd, "turbo ") || strings.HasPrefix(cmd, "nx "):
		out = compress.CompressNpm(lines)
	case strings.HasPrefix(cmd, "docker"):
		out = compress.CompressDocker(lines)
	case strings.HasPrefix(cmd, "kubectl"):
		out = compress.CompressKubectl(lines)
	case strings.HasPrefix(cmd, "make") || strings.HasPrefix(cmd, "gmake"):
		out = compress.CompressMake(lines)
	case strings.HasPrefix(cmd, "gh "):
		out = compress.CompressGh(lines)
	case strings.HasPrefix(cmd, "pip ") || strings.HasPrefix(cmd, "pip3 ") ||
		strings.HasPrefix(cmd, "uv ") || strings.HasPrefix(cmd, "conda ") ||
		strings.HasPrefix(cmd, "mamba ") || strings.HasPrefix(cmd, "pipx "):
		out = compress.CompressPip(lines)
	case strings.HasPrefix(cmd, "terraform") || strings.HasPrefix(cmd, "tofu"):
		out = compress.CompressTerraform(lines)
	case strings.HasPrefix(cmd, "cmake") || strings.HasPrefix(cmd, "ninja") ||
		strings.HasPrefix(cmd, "gcc ") || strings.HasPrefix(cmd, "g++ ") ||
		strings.HasPrefix(cmd, "cc "):
		out = compress.CompressCmake(lines)
	case strings.HasPrefix(cmd, "grep ") || strings.HasPrefix(cmd, "rg ") ||
		strings.HasPrefix(cmd, "ag ") || strings.HasPrefix(cmd, "ack "):
		out = compress.CompressGrep(lines)
	case strings.HasPrefix(cmd, "find ") || strings.HasPrefix(cmd, "fd "):
		out = compress.CompressFind(lines)
	case strings.HasPrefix(cmd, "eslint") || strings.HasPrefix(cmd, "npx eslint") ||
		strings.HasPrefix(cmd, "biome") || strings.HasPrefix(cmd, "hadolint") ||
		strings.HasPrefix(cmd, "yamllint") || strings.HasPrefix(cmd, "markdownlint") ||
		strings.HasPrefix(cmd, "oxlint"):
		out = compress.CompressEslint(lines)
	case strings.HasPrefix(cmd, "ruff"):
		out = compress.CompressRuff(cmd, lines)
	case strings.HasPrefix(cmd, "mypy") ||
		strings.HasPrefix(cmd, "pyright") || strings.HasPrefix(cmd, "basedpyright"):
		out = compress.CompressMypy(lines)
	case strings.HasPrefix(cmd, "pytest") || strings.HasPrefix(cmd, "python -m pytest") ||
		strings.HasPrefix(cmd, "python3 -m pytest") || strings.HasPrefix(cmd, "vitest") ||
		strings.HasPrefix(cmd, "jest") || strings.HasPrefix(cmd, "mocha") ||
		strings.HasPrefix(cmd, "jasmine"):
		out = compress.CompressPytest(lines)
	case strings.HasPrefix(cmd, "tsc") || strings.HasPrefix(cmd, "npx tsc"):
		out = compress.CompressTsc(lines)
	case strings.HasPrefix(cmd, "npx playwright") || strings.HasPrefix(cmd, "playwright") ||
		strings.HasPrefix(cmd, "npx cypress") || strings.HasPrefix(cmd, "cypress"):
		out = compress.CompressPlaywright(cmd, lines)
	case strings.HasPrefix(cmd, "next build") || strings.HasPrefix(cmd, "npx next build") ||
		strings.HasPrefix(cmd, "vite build") || strings.HasPrefix(cmd, "npx vite build"):
		out = compress.CompressNextBuild(cmd, lines)
	case strings.HasPrefix(cmd, "helm "):
		out = compress.CompressHelm(cmd, lines)
	case strings.HasPrefix(cmd, "ansible") || strings.HasPrefix(cmd, "ansible-playbook"):
		out = compress.CompressAnsible(lines)
	case strings.HasPrefix(cmd, "mvn ") || strings.HasPrefix(cmd, "./mvnw ") ||
		strings.HasPrefix(cmd, "mvnw ") || strings.HasPrefix(cmd, "gradle ") ||
		strings.HasPrefix(cmd, "./gradlew ") || strings.HasPrefix(cmd, "gradlew "):
		out = compress.CompressMaven(cmd, lines)
	case strings.HasPrefix(cmd, "bazel "):
		out = compress.CompressBazel(cmd, lines)
	case strings.HasPrefix(cmd, "poetry "):
		out = compress.CompressPoetry(cmd, lines)
	case strings.HasPrefix(cmd, "npx prisma ") || strings.HasPrefix(cmd, "prisma "):
		out = compress.CompressPrisma(cmd, lines)
	case strings.HasPrefix(cmd, "prettier ") || strings.HasPrefix(cmd, "npx prettier "):
		out = compress.CompressPrettier(lines)
	case strings.HasPrefix(cmd, "rubocop") || strings.HasPrefix(cmd, "bundle ") ||
		strings.HasPrefix(cmd, "rake ") || strings.HasPrefix(cmd, "rails "):
		out = compress.CompressRuby(cmd, lines)
	case strings.HasPrefix(cmd, "composer "):
		out = compress.CompressComposer(cmd, lines)
	case strings.HasPrefix(cmd, "php artisan "):
		out = compress.CompressArtisan(cmd[4:], lines)
	case strings.HasPrefix(cmd, "mix "):
		out = compress.CompressMix(cmd, lines)
	case strings.HasPrefix(cmd, "swift "):
		out = compress.CompressSwiftBuild(cmd, lines)
	case strings.HasPrefix(cmd, "zig "):
		out = compress.CompressZig(cmd, lines)
	case strings.HasPrefix(cmd, "ps ") || cmd == "ps":
		if c := compress.CompressPs(lines); c != nil {
			out = c
		}
	case strings.HasPrefix(cmd, "du ") || cmd == "du":
		if c := compress.CompressDu(lines); c != nil {
			out = c
		}
	case strings.HasPrefix(cmd, "ping "):
		if c := compress.CompressPing(lines); c != nil {
			out = c
		}
	case strings.HasPrefix(cmd, "systemctl ") || cmd == "systemctl" ||
		strings.HasPrefix(cmd, "journalctl"):
		out = compress.CompressSystemd(cmd, lines)
	case cmd == "ls" || strings.HasPrefix(cmd, "ls ") || strings.HasPrefix(cmd, "ls -"):
		if c := compress.CompressLs(lines); c != nil {
			out = c
		}
	case strings.HasPrefix(cmd, "mysql ") || cmd == "mysql" ||
		strings.HasPrefix(cmd, "mariadb "):
		out = compress.CompressMySQL(cmd, lines)
	case strings.HasPrefix(cmd, "psql ") || cmd == "psql":
		out = compress.CompressPsql(cmd, lines)
	case strings.HasPrefix(cmd, "env") || cmd == "env" || cmd == "printenv" ||
		strings.HasPrefix(cmd, "printenv ") || cmd == "export" ||
		strings.HasPrefix(cmd, "export "):
		out = compress.CompressEnvFilter(lines)
	default:
		if blocked := compress.CompressLogBlock(lines); blocked != nil {
			out = blocked
		} else if dedupd := compress.CompressLogDedup(lines); dedupd != nil {
			out = dedupd
		} else {
			out = compress.CompressGeneric(lines)
		}
	}

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
		return output, originalLines, originalLines
	}

	// Over-compression guard: >95% token reduction on small output is almost
	// always signal loss (e.g. compressing a one-line compiler error to nothing).
	origTok := compress.EstimateTokens(stripped)
	if origTok > 100 && origTok < 2000 {
		if float64(compress.EstimateTokens(strings.Join(out, "\n")))/float64(origTok) < 0.05 {
			return output, originalLines, originalLines
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
