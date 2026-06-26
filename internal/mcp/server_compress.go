package mcp

import (
	"fmt"
	"strings"

	"github.com/alehatsman/dex/internal/compress"
)

// minCompressLines is the minimum number of lines required before any pattern
// runs — tiny outputs gain nothing and can only be made worse.
const minCompressLines = 5

// CompressMinimal applies the light Minimal tier (#616): drop only provably-
// redundant noise (git index-hash plumbing, blank runs, non-signal duplicate
// lines) while preserving every diff/error/count line. Input is expected to be
// already ANSI-stripped and secret-masked (the shell path passes `clean`); the
// result is never longer than the input. Returns the text plus line counts.
func CompressMinimal(output string) (compressed string, originalLines, outputLines int) {
	if output == "" {
		return "", 0, 0
	}
	lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
	originalLines = len(lines)
	out := compress.Minimal(lines)
	// shorter_only guard: Minimal only ever removes lines, but stay explicit so
	// the metrics never report an expansion.
	if len(out) >= originalLines {
		return strings.Join(lines, "\n"), originalLines, originalLines
	}
	return strings.Join(out, "\n"), originalLines, len(out)
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
	out := compress.Dispatch(cmd, lines)
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
