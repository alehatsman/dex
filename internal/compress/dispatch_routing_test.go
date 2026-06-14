package compress

import "testing"

// TestDispatchFallbackForUnknown verifies an unrecognised command still yields
// non-nil output via the generic fallback rather than dropping through.
func TestDispatchFallbackForUnknown(t *testing.T) {
	if got := Dispatch("totally unknown cmd xyz", []string{"a", "b", "c", "d", "e"}); got == nil {
		t.Fatal("Dispatch returned nil for unknown command; expected generic fallback output")
	}
}

// TestRegisterWinsByMatchAndOrder verifies the registration interface: a
// compressor added via Register participates in dispatch, matches by its
// predicate, and a nil return falls through to the next handler.
func TestRegisterWinsByMatchAndOrder(t *testing.T) {
	saved := registry
	t.Cleanup(func() { registry = saved })

	sentinel := []string{"HANDLED"}
	Register(Compressor{
		Name:     "test-sentinel",
		Match:    word("dexselftest"),
		Compress: func(_ string, _ []string) []string { return sentinel },
	})

	got := Dispatch("dexselftest --flag", []string{"x", "y", "z", "1", "2", "3"})
	if len(got) != 1 || got[0] != "HANDLED" {
		t.Fatalf("registered compressor did not win: got %v", got)
	}

	// Word-bounded match must not fire on a longer token.
	if got := Dispatch("dexselftestextra", []string{"x", "y", "z", "1", "2", "3"}); len(got) == 1 && got[0] == "HANDLED" {
		t.Fatal("word predicate matched a longer token; expected no match")
	}
}

// TestDispatchFallsThroughOnNil verifies a matched compressor that returns nil
// declines the output and routing continues to the fallback.
func TestDispatchFallsThroughOnNil(t *testing.T) {
	saved := registry
	t.Cleanup(func() { registry = saved })

	called := false
	registry = []Compressor{{
		Name:  "decliner",
		Match: func(string) bool { return true },
		Compress: func(_ string, _ []string) []string {
			called = true
			return nil // decline
		},
	}}

	got := Dispatch("anything", []string{"a", "b", "c", "d", "e"})
	if !called {
		t.Fatal("declining compressor was never invoked")
	}
	if got == nil {
		t.Fatal("expected fallback output after decline, got nil")
	}
}
