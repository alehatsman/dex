// Package redact masks credential-shaped substrings in text before it is
// surfaced to a caller (e.g. shell output). Patterns preserve a leading
// context prefix (key=, bearer , etc.) so the surrounding text survives while
// the secret value itself is hidden.
package redact

import "regexp"

// rule pairs a pattern name with its compiled regex and replacement.
// Group 1 in the pattern captures the prefix to preserve (key=, bearer , etc.)
// so the replacement keeps context while hiding the credential value.
type rule struct {
	name string
	re   *regexp.Regexp
	repl string
}

var rules = []rule{
	{
		name: "Bearer token",
		re:   regexp.MustCompile(`(?i)(bearer\s+)[a-zA-Z0-9\-_\.]{8,}`),
		repl: "${1}[REDACTED:Bearer token]",
	},
	{
		name: "Authorization header",
		re:   regexp.MustCompile(`(?i)(authorization:\s*(?:basic|bearer|token)\s+)\S+`),
		repl: "${1}[REDACTED:Authorization header]",
	},
	{
		name: "API key param",
		re:   regexp.MustCompile(`(?i)((?:api[_\-]?key|apikey|access[_\-]?key|secret[_\-]?key|password|passwd|pwd|secret)\s*[=:]\s*)[^\s,;&"']{8,}`),
		repl: "${1}[REDACTED:secret]",
	},
	{
		name: "AWS access key",
		re:   regexp.MustCompile(`AKIA[0-9A-Z]{12,}`),
		repl: "[REDACTED:AWS key]",
	},
	{
		name: "GitHub token",
		re:   regexp.MustCompile(`(gh[pousr]_)[a-zA-Z0-9]{20,}`),
		repl: "${1}[REDACTED:GitHub token]",
	},
	{
		// Any PEM private-key block — RSA, EC, DSA, OPENSSH (the modern
		// ssh-keygen default), ENCRYPTED, plain PKCS#8, or PGP. The label
		// between BEGIN/END and "PRIVATE KEY" varies; `[A-Z0-9 ]*` absorbs it.
		name: "Private key block",
		re:   regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----[\s\S]*?-----END [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----`),
		repl: "[REDACTED:Private key]",
	},
	{
		name: "Slack token",
		re:   regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`),
		repl: "[REDACTED:Slack token]",
	},
	{
		name: "Google API key",
		re:   regexp.MustCompile(`AIza[0-9A-Za-z_\-]{35}`),
		repl: "[REDACTED:Google API key]",
	},
	{
		name: "Stripe key",
		re:   regexp.MustCompile(`(?:sk|rk)_(?:live|test)_[0-9a-zA-Z]{20,}`),
		repl: "[REDACTED:Stripe key]",
	},
	{
		// OpenAI / Anthropic API keys — the most dangerous to leak (the model's
		// own credential). Word-boundary-anchored so it can't eat the tail of a
		// hyphenated word like "task-management-…".
		name: "AI provider key",
		re:   regexp.MustCompile(`\bsk-(?:proj-)?[A-Za-z0-9_\-]{20,}`),
		repl: "[REDACTED:AI provider key]",
	},
	{
		name: "GitHub fine-grained PAT",
		re:   regexp.MustCompile(`github_pat_[A-Za-z0-9_]{82}`),
		repl: "[REDACTED:GitHub token]",
	},
	{
		name: "GitLab token",
		re:   regexp.MustCompile(`glpat-[A-Za-z0-9_\-]{20,}`),
		repl: "[REDACTED:GitLab token]",
	},
	{
		name: "SendGrid key",
		re:   regexp.MustCompile(`SG\.[A-Za-z0-9_\-]{22,}\.[A-Za-z0-9_\-]{16,}`),
		repl: "[REDACTED:SendGrid key]",
	},
	{
		name: "AWS STS key",
		re:   regexp.MustCompile(`ASIA[0-9A-Z]{16}`),
		repl: "[REDACTED:AWS STS key]",
	},
	{
		// Matches key=<32+char secret>, token=<secret>, etc.
		name: "Generic long secret",
		re:   regexp.MustCompile(`(?i)((?:key|token|secret|credential|auth)\s*[=:]\s*['"]?)[a-zA-Z0-9+/=\-_]{32,}['"]?`),
		repl: "${1}[REDACTED:secret]",
	},
}

// Mask replaces credential patterns in s with redaction markers. The prefix
// (e.g. "Bearer ", "api_key=") is preserved so context survives; only the
// credential value itself is masked.
func Mask(s string) string {
	for _, r := range rules {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	return s
}
