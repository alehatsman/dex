package redact_test

import (
	"strings"
	"testing"

	"github.com/alehatsman/dex/internal/ignore"
	"github.com/alehatsman/dex/internal/redact"
)

// TestSecretPanelsConsistent (#661) locks the two security panels in step on
// the set of secret TYPES they recognize, without forcing identical regexes:
//   - ignore.LooksLikeSecret decides whether a file is skipped at index time.
//   - redact.Mask scrubs the same secret from shell output before it reaches
//     the model.
//
// A type that one panel catches but the other misses is a leak (#659: redact
// missed OPENSSH keys; #660: redact missed OpenAI/GitLab/… that ignore caught).
// If a future change adds a secret type to one panel but not the other, this
// test fails. The panels keep their OWN regexes/thresholds on purpose — over-
// skipping a whole file (ignore) and over-masking a span (redact) have
// different false-positive costs, so they tune independently; this guards
// coverage, not wording.
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
		"Private key":      "-----BEGIN OPENSSH " + pk + "-----\n" + strings.Repeat("b", 40) + "\n-----END OPENSSH " + pk + "-----",
	}

	for name, secret := range samples {
		// ignore must flag a file containing it (so it is NOT indexed).
		if !ignore.LooksLikeSecret([]byte("config = " + secret + "\n")) {
			t.Errorf("ignore.LooksLikeSecret MISSES %q — a file containing it would be indexed", name)
		}
		// redact must mask it in output (so it does NOT reach the model).
		if masked := redact.Mask("value: " + secret); strings.Contains(masked, secret) {
			t.Errorf("redact.Mask LEAKS %q — shell output containing it would reach the model:\n  %q", name, masked)
		}
	}
}
