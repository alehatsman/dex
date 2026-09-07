package compress_test

import (
	"strings"
	"testing"

	"github.com/alehatsman/dex/internal/compress"
	"github.com/alehatsman/dex/internal/ignore"
)

// TestSecretPanelsConsistent (#661) locks the two security panels in step on
// the set of secret TYPES they recognize, without forcing identical regexes:
//   - ignore.LooksLikeSecret decides whether a file is skipped at index time.
//   - compress.RedactSecrets scrubs the same secret from command output before
//     it reaches the model, at the single chokepoint in CompressText
//     (internal/mcp/server_compress.go).
//
// A type that one panel catches but the other misses is a leak (#659: the
// output mask missed OPENSSH keys; #660: it missed OpenAI/GitLab/… that ignore
// caught). If a future change adds a secret type to one panel but not the
// other, this test fails. The panels keep their OWN regexes/thresholds on
// purpose — over-skipping a whole file (ignore) and over-masking a span
// (redact) have different false-positive costs, so they tune independently;
// this guards coverage, not wording.
//
// This guard used to point at internal/redact.Mask, which had zero production
// callers (#860): it governed a panel nothing ran, while the panel that
// actually masks output went ungoverned. It now targets the live one.
//
// Fixtures are built from fragments so no contiguous secret literal lands in
// source (keeps GitHub secret-scanning push protection from blocking, #659).
func TestSecretPanelsConsistent(t *testing.T) {
	pk := "PRIVATE " + "KEY"
	samples := map[string]string{
		"AWS access key":   "AKIA" + strings.Repeat("A", 16),
		"AWS STS key":      "ASIA" + strings.Repeat("B", 16),
		"OpenAI/Anthropic": "sk-proj-" + strings.Repeat("c", 24),
		"GitHub PAT":       "ghp_" + strings.Repeat("d", 36),
		"GitHub fine PAT":  "github_" + "pat_" + strings.Repeat("e", 82),
		"Slack token":      "xox" + "b-" + strings.Repeat("f", 20),
		"Google API key":   "AIza" + strings.Repeat("g", 35),
		"Stripe key":       "sk_" + "live_" + strings.Repeat("h", 24),
		"GitLab token":     "glpat-" + strings.Repeat("i", 24),
		"SendGrid key":     "SG." + strings.Repeat("j", 24) + "." + strings.Repeat("k", 20),
		"Private key":      "-----BEGIN OPENSSH " + pk + "-----",
	}

	for name, secret := range samples {
		// ignore must flag a file containing it (so it is NOT indexed).
		if !ignore.LooksLikeSecret([]byte("config = " + secret + "\n")) {
			t.Errorf("ignore.LooksLikeSecret MISSES %q — a file containing it would be indexed", name)
		}
		assertMasked(t, name+" under a key", "value: "+secret, secret)
		// Free-form, no key to anchor on: the auth-flow case that motivated the
		// third (recognizable-token) pass. A panel that only masks `key: value`
		// assignments still leaks a token pasted into prose or a device-code flow.
		assertMasked(t, name+" bare", secret, secret)
	}

	// Slack tokens span several subtype prefixes (bot/app/user/refresh/legacy).
	// The single-sample loop above only exercised one; a per-subtype sweep locks
	// the char-class so a future single-char drift between the two panels fails
	// the gate rather than silently leaking one subtype (#676: ignore's class had
	// drifted to xox[abps], dropping the xoxr- refresh token the mask still caught).
	for _, sub := range []string{"xoxb", "xoxa", "xoxp", "xoxr", "xoxs"} {
		secret := sub + "-" + strings.Repeat("f", 20)
		if !ignore.LooksLikeSecret([]byte("config = " + secret + "\n")) {
			t.Errorf("ignore.LooksLikeSecret MISSES Slack subtype %q — a file containing it would be indexed", sub)
		}
		assertMasked(t, "Slack subtype "+sub, "value: "+secret, secret)
	}
}

// assertMasked fails when the live output panel leaves secret intact in line.
// RedactSecrets is line-oriented and length-preserving, so the single-line
// slice round-trip is the whole contract.
func assertMasked(t *testing.T, name, line, secret string) {
	t.Helper()
	got := compress.RedactSecrets([]string{line})
	if len(got) != 1 {
		t.Fatalf("RedactSecrets(%q) returned %d lines, want 1 — the pass must never drop or split a line", name, len(got))
	}
	if strings.Contains(got[0], secret) {
		t.Errorf("compress.RedactSecrets LEAKS %s — command output containing it would reach the model:\n  %q", name, got[0])
	}
}
