package compress

import (
	"strings"
	"testing"
)

// #452: CompressPytest must keep the assertion/traceback detail, not just the
// FAILED count — the reason is what makes a failure actionable.
func TestCompressPytestKeepsAssertionDetail(t *testing.T) {
	in := []string{
		"============================= test session starts ==============================",
		"collected 3 items",
		"",
		"test_foo.py::test_add PASSED",
		"test_foo.py::test_sub FAILED",
		"",
		"=================================== FAILURES ===================================",
		"_______________________________ test_sub ________________________________",
		"",
		"    def test_sub():",
		">       assert 1 == 2",
		"E       assert 1 == 2",
		"",
		"test_foo.py:5: AssertionError",
		"=========================== short test summary info ============================",
		"FAILED test_foo.py::test_sub - assert 1 == 2",
		"======================== 1 failed, 2 passed in 0.10s ===========================",
	}

	out := CompressPytest(in)
	joined := strings.Join(out, "\n")

	for _, want := range []string{
		"1 failed, 2 passed",           // summary survives
		"FAILED test_foo.py::test_sub", // failed test name survives
		"E       assert 1 == 2",        // the assertion error detail survives
		">       assert 1 == 2",        // the failing source line survives
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("compressed output dropped %q\n--- got ---\n%s", want, joined)
		}
	}
}

// A clean run (no failure summary token) is left untouched.
func TestCompressPytestPassThroughWithoutSummary(t *testing.T) {
	in := []string{"hello", "world"}
	out := CompressPytest(in)
	if len(out) != len(in) {
		t.Fatalf("expected pass-through of %d lines, got %d: %v", len(in), len(out), out)
	}
}
