package compress

import (
	"strings"
	"testing"
)

func mini(s string) []string { return Minimal(strings.Split(s, "\n")) }

func TestMinimalDropsGitIndexLine(t *testing.T) {
	in := "diff --git a/x b/x\nindex 1a2b3c4..5d6e7f8 100644\n--- a/x\n+++ b/x"
	out := strings.Join(mini(in), "\n")
	if strings.Contains(out, "index 1a2b3c4") {
		t.Errorf("git index plumbing line should be dropped:\n%s", out)
	}
	for _, keep := range []string{"diff --git", "--- a/x", "+++ b/x"} {
		if !strings.Contains(out, keep) {
			t.Errorf("must keep %q:\n%s", keep, out)
		}
	}
}

func TestMinimalCollapsesBlankRuns(t *testing.T) {
	got := Minimal([]string{"a", "", "", "", "b"})
	want := []string{"a", "", "b"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("blank run not collapsed: %q", got)
	}
}

func TestMinimalDedupsNonSignalLines(t *testing.T) {
	got := Minimal([]string{"connecting...", "connecting...", "connecting...", "done"})
	want := []string{"connecting...", "done"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("consecutive dup not deduped: %q", got)
	}
}

// TestMinimalKeepsRepeatedDiffLines is the key correctness guard: two identical
// removed lines in a diff are distinct edits and BOTH must survive the dedup.
func TestMinimalKeepsRepeatedDiffLines(t *testing.T) {
	in := []string{"@@ -1,3 +1,1 @@", "-  x = 1", "-  x = 1", "+  x = 2"}
	got := Minimal(in)
	if strings.Join(got, "\n") != strings.Join(in, "\n") {
		t.Errorf("diff lines must be preserved verbatim, got:\n%s", strings.Join(got, "\n"))
	}
}

func TestMinimalPreservesSignalLines(t *testing.T) {
	// Repeated error / CVE / test-count lines are signal — never deduped.
	cases := [][]string{
		{"ERROR: build failed", "ERROR: build failed"},
		{"CVE-2024-1234 high", "CVE-2024-1234 high"},
		{"5 failed, 3 passed", "5 failed, 3 passed"},
	}
	for _, in := range cases {
		got := Minimal(in)
		if len(got) != 2 {
			t.Errorf("signal line wrongly deduped: in=%q got=%q", in, got)
		}
	}
}

func TestMinimalNeverLongerThanInput(t *testing.T) {
	in := []string{"index aaaaaaa..bbbbbbb 100644", "", "", "dup", "dup", "tail"}
	got := Minimal(in)
	if len(got) > len(in) {
		t.Errorf("Minimal produced more lines (%d) than input (%d)", len(got), len(in))
	}
}

func TestMinimalEmptyAndPlain(t *testing.T) {
	if got := Minimal(nil); len(got) != 0 {
		t.Errorf("nil input should yield empty, got %q", got)
	}
	in := []string{"single line"}
	if got := Minimal(in); len(got) != 1 || got[0] != "single line" {
		t.Errorf("plain single line altered: %q", got)
	}
}
