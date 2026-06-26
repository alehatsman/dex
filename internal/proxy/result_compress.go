package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
)

// shouldPreserveResult returns true when the tool_result content should be
// left byte-identical rather than replaced with a stub. Two cases:
//
//  1. The content is a dex tool result that was already compressed at
//     delivery time ("saved_pct" is present with a non-zero value; the
//     omitempty tag means it is absent when 0). These are compacted to a
//     short reference stub via compactDexStub rather than kept verbatim —
//     see rewriteBlock for the dispatch.
//
//  2. The content contains an <lc_safe> marker, which signals that the
//     surrounding bytes must reach the model verbatim — pruning them would
//     discard information the caller explicitly protected.
func shouldPreserveResult(text string) bool {
	if strings.Contains(text, `"saved_pct":`) {
		return true
	}
	if strings.Contains(text, "<lc_safe>") {
		return true
	}
	return false
}

// isDexResult returns true when text is a dex tool result carrying saved_pct,
// meaning it was already compressed at delivery time by the dex MCP server.
func isDexResult(text string) bool {
	return strings.Contains(text, `"saved_pct":`)
}

// compactDexStub produces a short reference stub for a dex tool result that
// has aged into the prune zone. Instead of keeping the full JSON verbatim
// (which can be thousands of tokens), the stub carries the key finding so the
// model can decide whether to re-query. Returns ("", false) when text is not
// recognisable dex JSON — caller should then preserve verbatim.
func compactDexStub(text, toolName string) (string, bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		return "", false // not JSON — preserve verbatim
	}

	const maxRunes = 200
	label := toolName
	if label == "" {
		label = "dex"
	}

	// ask / assemble: answer field carries the prose response.
	if raw, ok := m["answer"]; ok {
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			runes := []rune(s)
			ellipsis := ""
			if len(runes) > maxRunes {
				runes = runes[:maxRunes]
				ellipsis = "…"
			}
			return fmt.Sprintf("[earlier dex %s: %q%s (pruned; re-query if needed)]",
				label, string(runes), ellipsis), true
		}
	}

	// shell: output + exit_code.
	if raw, ok := m["output"]; ok {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			var exitCode int
			if ec, ok2 := m["exit_code"]; ok2 {
				_ = json.Unmarshal(ec, &exitCode)
			}
			firstLine := s
			if nl := strings.IndexByte(s, '\n'); nl >= 0 {
				firstLine = s[:nl]
			}
			runes := []rune(firstLine)
			ellipsis := ""
			if len(runes) > maxRunes {
				runes = runes[:maxRunes]
				ellipsis = "…"
			}
			return fmt.Sprintf("[earlier dex %s (exit=%d): %q%s (pruned; re-run if needed)]",
				label, exitCode, string(runes), ellipsis), true
		}
	}

	// find: count hits.
	if raw, ok := m["hits"]; ok {
		var hits []json.RawMessage
		if json.Unmarshal(raw, &hits) == nil {
			return fmt.Sprintf("[earlier dex %s: %d hits (pruned; re-search if needed)]",
				label, len(hits)), true
		}
	}

	// grep: count matches.
	if raw, ok := m["matches"]; ok {
		var matches []json.RawMessage
		if json.Unmarshal(raw, &matches) == nil {
			return fmt.Sprintf("[earlier dex %s: %d matches (pruned; re-search if needed)]",
				label, len(matches)), true
		}
	}

	// Generic fallback — still compacts from thousands of tokens to one line.
	return fmt.Sprintf("[earlier dex %s result (pruned; re-query if needed)]", label), true
}

// testOutputMarkers are substrings that, when found in tool_result content,
// signal test or build output whose error context is load-bearing. We keep
// these verbatim rather than applying head/tail summarisation, because a
// truncated compiler error or failing test name is often useless.
var testOutputMarkers = []string{
	"=== RUN",       // Go test runner
	"--- PASS",      // Go test runner
	"--- FAIL",      // Go test runner
	"FAIL\t",        // Go test runner package line
	"error[E",       // Rust compiler (error[E0xxx])
	"warning[",      // Rust compiler
	"test result:",  // Rust test runner
	"BUILD FAILED",  // Gradle / Maven
	"BUILD SUCCESS", // Gradle / Maven
}

// isTestOutput returns true when text looks like test or build runner output
// that must not be summarised.
func isTestOutput(text string) bool {
	for _, m := range testOutputMarkers {
		if strings.Contains(text, m) {
			return true
		}
	}
	return false
}
