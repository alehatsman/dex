package mcp

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/alehatsman/dex/internal/compress"
)

// reAnsi matches ANSI CSI/OSC escape sequences. stripANSI drops them before any
// compression or byte-capping so terminal control codes never inflate output or
// break the summary pipeline (relocated from the removed shell surface #197).
var reAnsi = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)`)

func stripANSI(s string) string { return reAnsi.ReplaceAllString(s, "") }

// authFlowStrong / authFlowWeak recognise an OAuth device-code prompt in
// captured output — such text must pass through uncompressed and untruncated so
// the user can still read the code and URL.
var authFlowStrong = []string{
	"devicelogin", "deviceauth", "device_code", "device code",
	"device-code", "verification_uri", "user_code", "one-time code",
}

var authFlowWeak = []string{
	"enter the code", "enter this code", "enter code:", "use the code",
	"use a web browser to open", "open the page",
	"authenticate by visiting", "sign in with the code",
	"sign in using a code", "verification code",
	"authorize this device", "waiting for authentication",
	"waiting for login", "open your browser", "open in your browser",
}

// containsAuthFlow returns true when output looks like an OAuth device-code
// flow. Output must never be compressed or truncated in this case.
func containsAuthFlow(output string) bool {
	lower := strings.ToLower(output)
	for _, s := range authFlowStrong {
		if strings.Contains(lower, s) {
			return true
		}
	}
	for _, s := range authFlowWeak {
		if strings.Contains(lower, s) {
			if strings.Contains(lower, "http://") || strings.Contains(lower, "https://") {
				return true
			}
		}
	}
	return false
}

// minCompressLines is the minimum number of lines required before any pattern
// runs — tiny outputs gain nothing and can only be made worse.
const minCompressLines = 5

// aggressiveMinLines and aggressiveMinBytes are the floor below which the lossy
// line-scoring passes (entropy + terse) are skipped. On small, non-redundant
// output those passes delete unique diagnostic lines — bare counts (`wc -l`,
// `grep -c`), exit codes, short tokens — that the caller has no way to know were
// dropped. That silent partial result is worse than a few extra tokens, and
// compression only pays off on large verbose output anyway. Both dimensions
// must be exceeded to run them (a wide-but-short or long-but-tiny output stays
// intact); the lossless command-specific summaries and dedup in Dispatch still
// run below the floor.
const (
	aggressiveMinLines = 50
	aggressiveMinBytes = 4096
)

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

	// The entropy and terse passes score and drop individual lines, so they can
	// silently delete unique, meaningful content from terse output. Only run
	// them once the output is large in BOTH lines and bytes — below the floor
	// the token savings are negligible and the silent-loss risk dominates.
	if originalLines >= aggressiveMinLines && len(stripped) >= aggressiveMinBytes {
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
