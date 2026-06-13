package compress

import (
	"strings"
	"testing"
)

func TestCompressBazelQuery_DoesNotMutateInput(t *testing.T) {
	const n = 40
	input := make([]string, n)
	for i := range input {
		input[i] = "//pkg:target" + strings.Repeat("x", i%3)
	}
	// Independent record of what the caller passed in.
	orig := append([]string(nil), input...)

	out := CompressBazelQuery(input)

	// The caller's slice must be untouched. append(input[:20], …) reuses
	// input's backing array (cap ≥ len(input)) and overwrites input[20]
	// with the summary line — the bug in #454.
	for i := range orig {
		if input[i] != orig[i] {
			t.Fatalf("CompressBazelQuery mutated caller input at %d: got %q, want %q", i, input[i], orig[i])
		}
	}

	// Output is the first 20 lines plus a single summary line.
	if len(out) != 21 {
		t.Fatalf("len(out) = %d, want 21", len(out))
	}
	for i := 0; i < 20; i++ {
		if out[i] != orig[i] {
			t.Errorf("out[%d] = %q, want %q", i, out[i], orig[i])
		}
	}
	if !strings.Contains(out[20], "more results") {
		t.Errorf("last line should be the summary, got %q", out[20])
	}
}

func TestCompressBazelQuery_ShortInputUnchanged(t *testing.T) {
	input := []string{"//a:a", "//b:b", "//c:c"}
	out := CompressBazelQuery(input)
	if len(out) != len(input) {
		t.Fatalf("len(out) = %d, want %d (<= 30 returns as-is)", len(out), len(input))
	}
	for i := range input {
		if out[i] != input[i] {
			t.Errorf("out[%d] = %q, want %q", i, out[i], input[i])
		}
	}
}
