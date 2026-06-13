package compress

import (
	"strings"
	"testing"
)

// #455: the failure-path compressors must retain the diagnostic detail (the
// assertion diff / expected-actual / traceback) — the part that says *why* a
// test failed — not just the summary count and the failure header line. One
// real failing-run fixture per runner; each asserts the reason survives.
func TestCompressorsRetainFailureDiagnostics(t *testing.T) {
	cases := []struct {
		name string
		fn   func([]string) []string
		in   []string
		want []string // substrings that MUST survive compression
	}{
		{
			name: "minitest",
			fn:   CompressMinitest,
			in: []string{
				"Run options: --seed 12345",
				"",
				"# Running:",
				"",
				"F",
				"",
				"Failure:",
				"CalcTest#test_add [test/calc_test.rb:8]:",
				"Expected: 4",
				"  Actual: 5",
				"",
				"  1) Failure:",
				"CalcTest#test_sub [test/calc_test.rb:14]:",
				"Expected: 1",
				"  Actual: 2",
				"",
				"2 runs, 2 assertions, 2 failures, 0 errors, 0 skips",
			},
			want: []string{
				"2 runs, 2 assertions, 2 failures", // summary survives
				"Expected: 1",                      // the diff reason survives
				"Actual: 2",
			},
		},
		{
			name: "swifttest",
			fn:   CompressSwiftTest,
			in: []string{
				"Test Suite 'All tests' started at 2026-06-13 10:00:00.000",
				"Test Case '-[CalcTests testAdd]' started.",
				"/src/Tests/CalcTests.swift:12: error: -[CalcTests testAdd] : XCTAssertEqual failed: (\"5\") is not equal to (\"4\")",
				"Test Case '-[CalcTests testAdd]' failed (0.001 seconds).",
				"Test Suite 'All tests' failed at 2026-06-13 10:00:00.100.",
				"\t Executed 1 test, with 1 failure (0 unexpected) in 0.001 seconds",
			},
			want: []string{
				"1 failed",              // summary survives
				"XCTAssertEqual failed", // the assertion reason survives
				"is not equal to",
			},
		},
		{
			name: "mypy",
			fn:   CompressMypy,
			in: []string{
				// no trailing [error-code] — mypy run without --show-error-codes
				"app/calc.py:7: error: Unsupported operand types for + (\"int\" and \"str\")",
				"app/calc.py:7: note: Left operand is of type \"int\"",
				"Found 1 error in 1 file (checked 3 source files)",
			},
			want: []string{
				"Found 1 error",             // summary survives
				"Unsupported operand types", // the codeless error reason survives
				"Left operand is of type",   // the note context survives
			},
		},
		{
			name: "artisantest",
			fn:   CompressArtisanTest,
			in: []string{
				"   PASS  Tests\\Unit\\ExampleTest",
				"  ✓ it adds",
				"",
				"   FAIL  Tests\\Unit\\CalcTest",
				"  ✕ it subtracts",
				"  Failed asserting that 2 matches expected 1.",
				"  --- Expected",
				"  +++ Actual",
				"  -1",
				"  +2",
				"  at tests/Unit/CalcTest.php:14",
				"",
				"Tests:  1 passed, 1 failed (2 assertions)",
				"Duration: 0.04s",
			},
			want: []string{
				"FAIL",                            // failed status survives
				"Failed asserting that 2 matches", // the assertion reason survives
				"tests/Unit/CalcTest.php:14",      // the file:line survives
			},
		},
		{
			name: "zigtest",
			fn:   CompressZigTest,
			in: []string{
				"Test [1/2] calc.test.add... OK",
				"Test [2/2] calc.test.sub... FAIL",
				"    expected 1, found 2",
				"    /src/calc.zig:14:5: 0x1037 in test.sub (test)",
				"        try testing.expectEqual(@as(i32, 1), sub(4, 2));",
				"1 passed; 0 skipped; 1 failed.",
			},
			want: []string{
				"1 failed",            // summary survives
				"expected 1, found 2", // the failure reason survives
				"calc.zig:14:5",       // the location survives
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := tc.fn(tc.in)
			joined := strings.Join(out, "\n")
			for _, want := range tc.want {
				if !strings.Contains(joined, want) {
					t.Errorf("%s: compressed output dropped %q\n--- got ---\n%s", tc.name, want, joined)
				}
			}
		})
	}
}
