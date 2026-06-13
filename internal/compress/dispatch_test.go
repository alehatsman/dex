package compress

import "testing"

// Regression: CompressGit (compactDiff) must not panic on empty lines in diff output.
func TestCompactDiffEmptyLine(t *testing.T) {
	input := []string{
		"diff --git a/foo.go b/foo.go",
		"--- a/foo.go",
		"+++ b/foo.go",
		"@@ -1,2 +1,2 @@",
		"",
		"+added line",
		"-removed line",
		"",
	}
	// Should not panic.
	got := compactDiff(input)
	if len(got) == 0 {
		t.Error("expected non-empty result")
	}
}
