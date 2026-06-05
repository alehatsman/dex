package store

import (
	"strings"
	"unicode"
)

// splitCamelCase splits a CamelCase or PascalCase identifier into sub-tokens
// at lowercase→uppercase transitions. The original token is NOT included;
// callers append it separately. Single-rune and single-char sub-tokens are
// dropped (they add noise without recall value).
//
//	"AuthService"   → ["Auth", "Service"]
//	"handleRequest" → ["handle", "Request"]
//	"parseHTTPURL"  → ["parse", "HTTPURL"]  (run of caps kept together)
//	"simple"        → []                    (no transitions → no expansion)
func splitCamelCase(s string) []string {
	runes := []rune(s)
	var parts []string
	start := 0
	for i := 1; i < len(runes); i++ {
		if unicode.IsUpper(runes[i]) && unicode.IsLower(runes[i-1]) {
			part := string(runes[start:i])
			if len([]rune(part)) >= 2 {
				parts = append(parts, part)
			}
			start = i
		}
	}
	if tail := string(runes[start:]); len([]rune(tail)) >= 2 && start > 0 {
		parts = append(parts, tail)
	}
	return parts
}

// expandCamelTerm wraps a single FTS5 phrase term (already quoted, e.g.
// `"AuthService"`) with OR alternatives for each CamelCase sub-token.
// Returns the original term unchanged when no expansion is possible.
//
//	`"AuthService"` → `("AuthService" OR "Auth" OR "Service")`
//	`"simple"`      → `"simple"`  (no CamelCase transitions)
func expandCamelTerm(term string) string {
	// Only expand single-word terms (no spaces inside the FTS phrase).
	inner := strings.Trim(term, `"`)
	if strings.ContainsAny(inner, " \t") {
		return term // phrase — don't expand
	}
	parts := splitCamelCase(inner)
	if len(parts) == 0 {
		return term
	}
	all := make([]string, 0, len(parts)+1)
	all = append(all, term)
	for _, p := range parts {
		all = append(all, `"`+p+`"`)
	}
	return "(" + strings.Join(all, " OR ") + ")"
}

