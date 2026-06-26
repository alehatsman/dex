package compress

import (
	"regexp"
	"strings"
)

// Minimal is the light-touch compression tier (#616): it sits between verbatim
// (preserve everything) and the full CompressText pass (aggressive pattern
// stripping). It is for structured, must-not-mangle output — git diff/log/show/
// blame, dependency audits — that is wasteful to send verbatim yet unsafe to
// compress aggressively (every +/- diff line, every error, every failure
// summary has to survive).
//
// It removes only provably-redundant noise:
//   - git "index <sha>..<sha>" object-id lines (pure plumbing, never read)
//   - runs of blank lines collapsed to a single blank
//   - consecutive identical lines collapsed to one — UNLESS the line carries
//     signal (a diff line, an error/warning, a CVE, a test count), because two
//     identical "-x" removed lines in a diff are distinct and both matter.
//
// Everything else is passed through unchanged. The result is never longer than
// the input, so callers can apply it unconditionally.
func Minimal(lines []string) []string {
	out := make([]string, 0, len(lines))
	prevBlank := false
	prevEmitted := "" // the immediately preceding emitted line, for dedup
	for _, ln := range lines {
		if reGitIndexLine.MatchString(ln) {
			continue // drop plumbing
		}
		if strings.TrimSpace(ln) == "" {
			if prevBlank {
				continue // collapse blank run
			}
			prevBlank = true
			prevEmitted = ln
			out = append(out, ln)
			continue
		}
		prevBlank = false
		// Drop an exact consecutive duplicate, but only when the line carries no
		// signal — a repeated diff/error/count line must be kept verbatim.
		if ln == prevEmitted && !minimalMustPreserve(ln) {
			continue
		}
		prevEmitted = ln
		out = append(out, ln)
	}
	return out
}

var (
	// reGitIndexLine matches the "index 1a2b3c4..5d6e7f8 100644" object-id line
	// git diff emits per file — pure plumbing an agent never needs.
	reGitIndexLine = regexp.MustCompile(`^index [0-9a-f]{7,40}\.\.[0-9a-f]{7,40}`)
	// reMinimalSignal flags a line that must be preserved verbatim even if it
	// repeats: diff markers, errors/warnings, CVE IDs, test counts, severities.
	reCVE          = regexp.MustCompile(`CVE-\d{4}-\d{3,}`)
	reTestCount    = regexp.MustCompile(`(?i)\b\d+\s+(passed|failed|skipped|errors?|warnings?|deselected|xfailed|xpassed|vulnerabilit(?:y|ies))\b`)
	reMinimalWords = regexp.MustCompile(`(?i)\b(error|warn|warning|fail|failed|failure|pass|passed|critical|high|moderate|severe)\b`)
)

// minimalMustPreserve reports whether a line carries signal that must survive
// the dedup pass: a diff marker (+/-/@@), an error/warning/severity word, a CVE
// id, or a test/vuln count.
func minimalMustPreserve(line string) bool {
	if n := len(line); n > 0 {
		switch line[0] {
		case '+', '-':
			return true
		}
		if strings.HasPrefix(line, "@@") {
			return true
		}
	}
	return reMinimalWords.MatchString(line) || reCVE.MatchString(line) || reTestCount.MatchString(line)
}
