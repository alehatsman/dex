package store

import (
	"strings"
	"unicode"
)

// spladesExpansions is a static programming-term synonym dictionary.
// Keys are lowercased query tokens; values are related terms added as
// OR alternatives in the FTS5 MATCH expression (SPLADE-inspired recall
// boost without a model). Only high-confidence associations are listed;
// broad/noisy terms are intentionally omitted.
var spladesExpansions = map[string][]string{
	"auth":       {"authentication", "token", "jwt", "login", "session", "oauth", "credential", "permission", "middleware"},
	"async":      {"await", "goroutine", "channel", "concurrent", "future", "promise"},
	"error":      {"err", "result", "exception", "panic", "fault"},
	"http":       {"request", "response", "handler", "rest", "json", "header", "endpoint"},
	"db":         {"database", "sql", "query", "transaction", "migration", "schema", "store"},
	"test":       {"mock", "fixture", "assert", "stub", "spec", "suite"},
	"cache":      {"redis", "ttl", "invalidate", "evict", "memcache"},
	"config":     {"configuration", "env", "settings", "flag", "option"},
	"log":        {"logger", "logging", "trace", "debug", "warn"},
	"deploy":     {"deployment", "release", "rollout", "version", "manifest"},
	"server":     {"handler", "listener", "endpoint", "service", "rpc", "serve"},
	"client":     {"connection", "dial", "connect", "transport"},
	"stream":     {"channel", "pipe", "buffer", "reader", "writer"},
	"file":       {"path", "dir", "directory", "io"},
	"net":        {"network", "tcp", "udp", "socket", "conn"},
	"msg":        {"message", "event", "payload", "packet"},
	"index":      {"idx", "key", "lookup", "table"},
	"parse":      {"decode", "unmarshal", "read", "scan", "lex"},
	"encode":     {"marshal", "serialize", "write", "format"},
	"compress":   {"compression", "gzip", "zstd", "deflate", "minify"},
	"search":     {"query", "lookup", "find", "match", "rank", "retrieve"},
	"embed":      {"embedding", "vector", "semantic", "similarity", "encode"},
	"chunk":      {"segment", "block", "slice", "window", "split"},
	"graph":      {"node", "edge", "dependency", "import", "call", "tree"},
	"queue":      {"channel", "buffer", "fifo", "dequeue", "enqueue", "worker"},
	"metric":     {"counter", "gauge", "histogram", "stats", "telemetry"},
	"middleware":  {"interceptor", "filter", "handler", "chain", "wrapper"},
}

// expandSPLADE wraps a camelCase-expanded FTS5 term with additional OR
// alternatives from the static synonym dictionary when the lowercased
// source token has a known entry. Returns camelExpr unchanged when no
// expansion applies.
//
//	"auth"  → ("auth" OR "authentication" OR "token" OR "jwt" OR ...)
//	"cache" → ("cache" OR "redis" OR "ttl" OR ...)
//	"foo"   → "foo"  (no expansion)
func expandSPLADE(lowerToken, camelExpr string) string {
	synonyms, ok := spladesExpansions[lowerToken]
	if !ok {
		return camelExpr
	}
	parts := make([]string, 0, len(synonyms)+1)
	parts = append(parts, camelExpr)
	for _, s := range synonyms {
		parts = append(parts, `"`+s+`"`)
	}
	return "(" + strings.Join(parts, " OR ") + ")"
}

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

