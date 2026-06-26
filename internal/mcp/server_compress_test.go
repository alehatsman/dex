package mcp

import (
	"strings"
	"testing"
)

// TestCompressMinimal covers the Minimal-tier wrapper (#616): it reports honest
// line counts, drops git index plumbing + blank runs, and keeps diff content.
func TestCompressMinimal(t *testing.T) {
	in := strings.Join([]string{
		"diff --git a/x.go b/x.go",
		"index 1a2b3c4..5d6e7f8 100644",
		"--- a/x.go",
		"+++ b/x.go",
		"@@ -1,2 +1,2 @@",
		"-old line",
		"+new line",
		"",
		"",
		"",
	}, "\n")

	out, orig, outLines := CompressMinimal(in)

	if orig <= outLines {
		t.Errorf("expected fewer output lines than input: orig=%d out=%d", orig, outLines)
	}
	if strings.Contains(out, "index 1a2b3c4") {
		t.Errorf("git index line should be dropped:\n%s", out)
	}
	for _, keep := range []string{"-old line", "+new line", "@@ -1,2 +1,2 @@"} {
		if !strings.Contains(out, keep) {
			t.Errorf("must keep diff line %q:\n%s", keep, out)
		}
	}
}

func TestCompressMinimalEmpty(t *testing.T) {
	out, orig, outLines := CompressMinimal("")
	if out != "" || orig != 0 || outLines != 0 {
		t.Errorf("empty input should yield zeros, got %q %d %d", out, orig, outLines)
	}
}
