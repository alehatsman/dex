package index

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestLooksMinified(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want bool
	}{
		{
			name: "short single line is never flagged",
			data: []byte("const x=" + string(bytes.Repeat([]byte("a"), 200))),
			want: false, // below minifiedMinBytes
		},
		{
			name: "large single-line bundle",
			data: bytes.Repeat([]byte("a"), 32*1024), // one 32 KB line
			want: true,
		},
		{
			name: "large but normally-wrapped source",
			// 32 KB of 40-char lines → avg well under the threshold.
			data: bytes.Repeat([]byte("func foo() { return bar(baz, qux) }\n"), 1000),
			want: false,
		},
		{
			name: "large pretty-printed data (normal lines) not minified",
			data: bytes.Repeat([]byte("  \"key\": \"value\",\n"), 2000),
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := LooksMinified(c.data); got != c.want {
				t.Errorf("LooksMinified() = %v, want %v (len=%d)", got, c.want, len(c.data))
			}
		})
	}
}

func TestLoadChunkGuardDefaults(t *testing.T) {
	// No .dex/config.yml → defaults.
	guard := LoadChunkGuard(t.TempDir())
	if guard.MaxChunksPerFile != DefaultMaxChunksPerFile {
		t.Errorf("MaxChunksPerFile = %d, want default %d", guard.MaxChunksPerFile, DefaultMaxChunksPerFile)
	}
	if !guard.SkipMinified {
		t.Error("SkipMinified = false, want default true")
	}
}

func TestLoadChunkGuardOverrides(t *testing.T) {
	root := t.TempDir()
	writeGuardConfig(t, root, "index:\n  max_chunks_per_file: 42\n  skip_minified: false\n")
	guard := LoadChunkGuard(root)
	if guard.MaxChunksPerFile != 42 {
		t.Errorf("MaxChunksPerFile = %d, want 42", guard.MaxChunksPerFile)
	}
	if guard.SkipMinified {
		t.Error("SkipMinified = true, want false (explicit override)")
	}
}

func TestLoadChunkGuardDisableCap(t *testing.T) {
	root := t.TempDir()
	writeGuardConfig(t, root, "index:\n  max_chunks_per_file: -1\n")
	guard := LoadChunkGuard(root)
	if guard.MaxChunksPerFile != -1 {
		t.Errorf("MaxChunksPerFile = %d, want -1 (disabled)", guard.MaxChunksPerFile)
	}
	// skip_minified unset → stays default on.
	if !guard.SkipMinified {
		t.Error("SkipMinified = false, want default true when unset")
	}
}

func TestLoadChunkGuardIgnoresUnrelatedKeys(t *testing.T) {
	// A config with only include/ignore (the ignore package's keys) must not
	// perturb the guard defaults.
	root := t.TempDir()
	writeGuardConfig(t, root, "index:\n  include: [\"cmd/\"]\n  ignore: [\"testdata/\"]\n")
	guard := LoadChunkGuard(root)
	if guard.MaxChunksPerFile != DefaultMaxChunksPerFile || !guard.SkipMinified {
		t.Errorf("guard = %+v, want defaults", guard)
	}
}

func writeGuardConfig(t *testing.T, root, body string) {
	t.Helper()
	dir := filepath.Join(root, ".dex")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
