package compress

import (
	"regexp"
	"strings"

	"github.com/alehatsman/dex/internal/ignore"
)

// ── uniform secret redaction (#461) ─────────────────────────────────────────────
//
// CompressEnvFilter redacts secrets only for `env`-shaped output. Every other
// command path (kubectl secret YAML, SQL rows with password/token columns, and
// the auth-flow whole-output early return) flowed through CompressText
// untouched and leaked credentials into the model context. RedactSecrets is the
// single value-level scrub applied at that chokepoint so the policy is uniform
// regardless of which command produced the output.
//
// It is deliberately conservative — it never drops or reorders lines, so
// compression ratios and anchor preservation for non-secret content are
// unchanged. Only the secret span within a line is replaced with "***".

// redactKeyValRe matches a `key: value` or `key = value` assignment, capturing
// the key (group 1), the separator span including surrounding spaces (group 2),
// and the value (group 3). It anchors on the first separator so YAML
// (`password: cGFzcw==`) and shell/env (`API_KEY=abc`) assignments both match.
// Leading whitespace and list/table markers ("- ", "| ") are tolerated.
var redactKeyValRe = regexp.MustCompile(`^(\s*(?:[-|]\s*)?[A-Za-z0-9_.\-]+)(\s*[:=]\s*)(.+?)\s*$`)

// RedactSecrets scrubs secret values from already-split output lines, returning
// a new slice with the same length and ordering. Three value-level passes run
// on every line:
//
//  1. credential URLs — the password span of any scheme://user:pass@host
//     connection string is masked (reusing the #460 regex), keeping the rest of
//     the URL useful.
//  2. denylisted keys — a `key: value` / `key=value` assignment whose key
//     contains a sensitive token (KEY/SECRET/TOKEN/PASSWORD/… — the env
//     denylist) has its value masked. This covers kubectl secret YAML and SQL
//     column output rendered as key/value.
//  3. recognizable tokens — any well-known secret token (GitHub PAT, AWS key,
//     JWT-style API key, …) is masked in place via ignore.RedactSecretTokens,
//     catching secrets that leak under benign keys or in free-form auth-flow
//     text with no key at all.
//
// Non-secret content is returned byte-for-byte unchanged.
func RedactSecrets(lines []string) []string {
	if len(lines) == 0 {
		return lines
	}
	out := make([]string, len(lines))
	for i, line := range lines {
		out[i] = redactLine(line)
	}
	return out
}

func redactLine(line string) string {
	// (1) credential URL password span.
	line = envCredURLRe.ReplaceAllString(line, "${1}***${2}")

	// (2) denylisted key/value assignment.
	if m := redactKeyValRe.FindStringSubmatch(line); m != nil {
		keyUpper := strings.ToUpper(m[1])
		for _, p := range envSensitivePatterns {
			if strings.Contains(keyUpper, p) {
				return m[1] + m[2] + "***"
			}
		}
	}

	// (3) recognizable secret tokens anywhere in the line.
	return ignore.RedactSecretTokens(line)
}
