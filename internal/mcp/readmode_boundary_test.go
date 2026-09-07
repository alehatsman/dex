package mcp

import "testing"

// TestSummarizeResolveModeNormalizesEverySource (#869) pins that all three
// sources of the read mode go through ParseReadMode.
//
// The request mode was normalized inline while the profile default and the task
// override were cast raw, so a capitalized or padded value from a hand-edited
// .dex/config.yml became a ReadMode that ValidReadMode then rejected — with the
// parser that exists to absorb exactly that never being called.
func TestSummarizeResolveModeNormalizesEverySource(t *testing.T) {
	s := &Server{}
	for _, raw := range []string{"Signatures", "  signatures  ", "SIGNATURES", "signatures"} {
		mode, isLLM := s.summarizeResolveMode(t.Context(), SummarizeInput{Mode: raw})
		if mode != ReadModeSignatures {
			t.Errorf("Mode=%q resolved to %q; want %q — the wire string must be normalized at the boundary",
				raw, mode, ReadModeSignatures)
		}
		if !ValidReadMode(mode) {
			t.Errorf("Mode=%q produced an invalid ReadMode %q", raw, mode)
		}
		if isLLM {
			t.Errorf("Mode=%q reported isLLM; only %q is an LLM path", raw, ReadModeSummary)
		}
	}

	// Empty and whitespace-only fall to the default rather than producing "".
	for _, raw := range []string{"", "   ", "\t\n"} {
		if mode, _ := s.summarizeResolveMode(t.Context(), SummarizeInput{Mode: raw}); mode != ReadModeFull {
			t.Errorf("Mode=%q resolved to %q; want the %q default", raw, mode, ReadModeFull)
		}
	}

	// summary stays the one LLM path after normalization.
	if _, isLLM := s.summarizeResolveMode(t.Context(), SummarizeInput{Mode: " Summary "}); !isLLM {
		t.Error(`Mode=" Summary " did not resolve to the LLM path`)
	}
}
