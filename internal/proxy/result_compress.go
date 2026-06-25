package proxy

import "strings"

// shouldPreserveResult returns true when the tool_result content should be
// left byte-identical rather than replaced with a stub. Two cases:
//
//  1. The content is a dex tool result that was already compressed at
//     delivery time ("saved_pct" is present with a non-zero value; the
//     omitempty tag means it is absent when 0). Replacing these with a
//     re-read stub is actively worse than keeping the compressed view.
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
