package compress

import (
	"strings"
	"testing"
)

// TestCompressPoetry_KeepsFailureHeaders guards #462: the poetry noise filter
// must drop install/update progress lines but keep the failure header that
// names which package failed — the greedy unanchored patterns used to eat it.
func TestCompressPoetry_KeepsFailureHeaders(t *testing.T) {
	lines := []string{
		"Installing dependencies from lock file",
		"  • Installing charset-normalizer (3.3.2)",
		"  • Updating urllib3 (1.26.0 -> 2.2.1)",
		"  • Installing cryptography (42.0.5): Failed",
		"",
		"  RuntimeError: Failed to build cryptography",
		"  could not find Rust compiler",
	}
	out := CompressPoetry("poetry install", lines)
	joined := strings.Join(out, "\n")

	if !strings.Contains(joined, "Installing cryptography (42.0.5): Failed") {
		t.Errorf("failure header dropped as noise:\n%s", joined)
	}
	if !strings.Contains(joined, "could not find Rust compiler") {
		t.Errorf("diagnostic line dropped:\n%s", joined)
	}
	if strings.Contains(joined, "Installing charset-normalizer (3.3.2)") {
		t.Errorf("progress line should be dropped as noise:\n%s", joined)
	}
	if strings.Contains(joined, "Updating urllib3") {
		t.Errorf("update progress line should be dropped as noise:\n%s", joined)
	}
}

func TestIsPoetryDownloadNoise(t *testing.T) {
	cases := []struct {
		line  string
		noise bool
	}{
		{"  • Installing foo (1.0)", true},
		{"  • Updating bar (1.0 -> 2.0)", true},
		{"  42%", true},
		{"  • Installing foo (1.0): Failed", false}, // failure header — keep
		{"  • Installing foo (1.0): Error", false},
		{"RuntimeError installing the build backend", false},      // not a bullet line
		{"  could not build wheel (12% complete tracked)", false}, // % not leading
	}
	for _, c := range cases {
		if got := IsPoetryDownloadNoise(c.line); got != c.noise {
			t.Errorf("IsPoetryDownloadNoise(%q) = %v, want %v", c.line, got, c.noise)
		}
	}
}
