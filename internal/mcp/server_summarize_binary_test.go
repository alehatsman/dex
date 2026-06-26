package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSummarizeRefusesBinary locks #674: reading a binary file must report
// status="binary" with a clear hint and NO content, rather than dumping raw
// bytes (null bytes included) into the agent's context — token waste and
// potential transport corruption. dex skips binaries at index time; the read
// path mirrors that refusal.
func TestSummarizeRefusesBinary(t *testing.T) {
	root := t.TempDir()
	file := "blob.bin"
	// A PNG-ish header plus an embedded NUL run — unambiguously binary.
	data := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...)
	data = append(data, []byte("\x00\x01\x02\x03garble\x00\xff")...)
	if err := os.WriteFile(filepath.Join(root, file), data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	s := &Server{}
	ctx := context.Background()

	for _, mode := range []string{"full", "signatures", "lines:1-10"} {
		t.Run(mode, func(t *testing.T) {
			out, err := s.Summarize(ctx, SummarizeInput{Path: file, ProjectRoot: root, Mode: mode})
			if err != nil {
				t.Fatalf("Summarize returned a transport error: %v", err)
			}
			if out.Status != "binary" {
				t.Fatalf("status = %q, want \"binary\"", out.Status)
			}
			if out.Content != "" {
				t.Errorf("binary content leaked into Content (%d bytes)", len(out.Content))
			}
			if !strings.Contains(out.Hint, "binary file") {
				t.Errorf("hint = %q, want it to flag the binary file", out.Hint)
			}
			if out.Bytes != len(data) {
				t.Errorf("Bytes = %d, want %d", out.Bytes, len(data))
			}
		})
	}

	// A text file in the same project must still read normally — the guard is
	// content-gated, not a blanket refusal.
	t.Run("text still reads", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(root, "ok.go"), []byte("package x\n\nfunc A() {}\n"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		out, err := s.Summarize(ctx, SummarizeInput{Path: "ok.go", ProjectRoot: root, Mode: "full"})
		if err != nil {
			t.Fatalf("Summarize: %v", err)
		}
		if out.Status != "ok" {
			t.Fatalf("status = %q, want \"ok\" for a text file", out.Status)
		}
		if !strings.Contains(out.Content, "func A()") {
			t.Errorf("text content missing; got %q", out.Content)
		}
	})
}
