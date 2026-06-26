// Package gotcha turns a failed shell command into a low-confidence Gotcha
// candidate an agent can confirm into a persistent note (#601). Detection is
// pure and zero-inference — a static library of ~common failure signatures
// matched by regexp against the already-compressed command output — so it adds
// no LLM call and runs only when a command exits non-zero.
//
// The candidate is a SUGGESTION, never an auto-write: dex stays read-only on
// its own knowledge store from the shell path. The agent decides whether the
// pitfall is worth persisting via `notes add`, which is why the confidence is
// deliberately low.
package gotcha

import (
	"regexp"
	"strings"
)

// Candidate is a staged Gotcha an agent may confirm into a note. It is emitted
// inline on a failing shell call and is intentionally low-confidence — the
// agent must judge whether the pitfall generalizes before persisting it.
type Candidate struct {
	// Class is the failure family (build, test, permission, …) — a stable key
	// an agent or a future dedup pass can group on.
	Class string `json:"class"`
	// Trigger is a short human label: what kind of failure this was.
	Trigger string `json:"trigger"`
	// OutputFragment is the single output line that matched, so the agent sees
	// the evidence without re-reading the whole output.
	OutputFragment string `json:"output_fragment"`
	// Archetype is always "Gotcha" — the note archetype this maps onto.
	Archetype string `json:"archetype"`
	// Confidence is low by design (the agent confirms). Stored as a hint.
	Confidence float64 `json:"confidence"`
	// Suggest is the ready-to-run note the agent can confirm.
	Suggest string `json:"suggest"`
}

// signature is one failure pattern: a compiled regexp, the class it maps to,
// and a human trigger label.
type signature struct {
	class   string
	trigger string
	re      *regexp.Regexp
}

// signatures is the static library, ordered most-specific first so a line that
// could match several (e.g. a missing-module build error also contains "build")
// is classified by the most informative pattern. Patterns are
// case-insensitive and matched per line against the compressed output.
var signatures = []signature{
	// ── build / compile ──────────────────────────────────────────────────
	{"build", "missing Go module/package", regexp.MustCompile(`(?i)(no required module provides package|cannot find module|cannot find package|missing go\.sum entry)`)},
	{"build", "undefined symbol", regexp.MustCompile(`(?i)undefined: \S`)},
	{"build", "compile error", regexp.MustCompile(`(?i)(syntax error|expected declaration|too many errors|cannot use .* as .* value)`)},
	{"build", "linker error", regexp.MustCompile(`(?i)(undefined reference to|ld: symbol|cannot find -l)`)},
	{"build", "unresolved import", regexp.MustCompile(`(?i)(modulenotfounderror|cannot find name|could not resolve|unresolved import|error\[E0432\])`)},
	// ── tests ────────────────────────────────────────────────────────────
	{"test", "test failure", regexp.MustCompile(`(?i)(^--- FAIL:|^FAIL\b|tests? failed|assertion(error)?|✗ )`)},
	{"panic", "runtime panic", regexp.MustCompile(`(?i)(^panic:|fatal error:|runtime error:|segmentation fault|nullpointerexception)`)},
	// ── environment / deps ───────────────────────────────────────────────
	{"missing-command", "command not found", regexp.MustCompile(`(?i)(command not found|executable file not found|: not found$|no such file or directory.*exec)`)},
	{"permission", "permission denied", regexp.MustCompile(`(?i)(permission denied|operation not permitted|eacces)`)},
	{"missing-file", "missing file/path", regexp.MustCompile(`(?i)no such file or directory`)},
	// ── network / auth ───────────────────────────────────────────────────
	{"network", "network failure", regexp.MustCompile(`(?i)(connection refused|no such host|i/o timeout|tls handshake timeout|network is unreachable|could not resolve host|temporary failure in name resolution)`)},
	{"auth", "authentication failure", regexp.MustCompile(`(?i)(401 unauthorized|403 forbidden|authentication failed|invalid credentials|permission to .* denied to)`)},
	// ── disk ─────────────────────────────────────────────────────────────
	{"disk", "out of space", regexp.MustCompile(`(?i)(no space left on device|disk quota exceeded)`)},
}

// maxScanLines caps how many output lines we scan so detection stays cheap even
// on a large failure dump; the signal is almost always near the tail of the
// output, so we scan the last maxScanLines lines.
const maxScanLines = 80

// Detect classifies a failed shell command into a Gotcha candidate, or returns
// nil when the command succeeded (exitCode == 0) or no known failure signature
// matched. A nil result is the common case and means "nothing worth staging" —
// callers omit the field entirely.
func Detect(command, output string, exitCode int) *Candidate {
	if exitCode == 0 {
		return nil
	}
	lines := tailLines(output, maxScanLines)
	for _, sig := range signatures {
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if sig.re.MatchString(line) {
				return newCandidate(command, sig, line)
			}
		}
	}
	return nil
}

// newCandidate builds the staged candidate for a matched signature.
func newCandidate(command string, sig signature, fragment string) *Candidate {
	fragment = truncate(fragment, 200)
	cmd := truncate(strings.TrimSpace(command), 120)
	return &Candidate{
		Class:          sig.class,
		Trigger:        sig.trigger,
		OutputFragment: fragment,
		Archetype:      "Gotcha",
		Confidence:     0.3,
		Suggest: "if this pitfall recurs, persist it: notes action=add archetype=Gotcha " +
			"body=\"`" + cmd + "` → " + sig.trigger + ": " + fragment + "\"",
	}
}

// tailLines returns the last n non-empty-trimmed lines of s (or all of them
// when there are fewer than n), preserving order.
func tailLines(s string, n int) []string {
	all := strings.Split(s, "\n")
	if len(all) <= n {
		return all
	}
	return all[len(all)-n:]
}

// truncate clips s to max runes, appending an ellipsis when it had to cut.
func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
