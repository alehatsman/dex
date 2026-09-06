package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidReadMode(t *testing.T) {
	valid := []string{
		"full", "signatures", "skeleton", "map", "aggressive", "summary",
		"lines:1-10", "lines:bad", // lines:* parsing is validated downstream
		"handle",
	}
	for _, m := range valid {
		if !ValidReadMode(ReadMode(m)) {
			t.Errorf("ValidReadMode(%q) = false, want true", m)
		}
	}
	invalid := []string{
		"entropy", // dex compress --mode=entropy's name, not a read mode (#854)
		"auto",    // dex compress --mode=auto's name, not a read mode (#854)
		"lines",   // bare stand-in, not dispatchable; needs lines:N-M
		"",        // empty is only valid as an *implicit* default, not explicit
		"xyzzy",
		"Full", // case-sensitive: resolve lowercases before this check
	}
	for _, m := range invalid {
		if ValidReadMode(ReadMode(m)) {
			t.Errorf("ValidReadMode(%q) = true, want false", m)
		}
	}
}

// TestSummarizeRejectsUnknownMode locks #528: an explicitly-requested mode the
// dispatch can't handle must error rather than silently serving the full raw
// file (a token blow-up).
func TestSummarizeRejectsUnknownMode(t *testing.T) {
	root := t.TempDir()
	file := "big.go"
	body := "package x\n" + strings.Repeat("// filler line\n", 500)
	if err := os.WriteFile(filepath.Join(root, file), []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	s := &Server{}
	ctx := context.Background()

	out, err := s.Summarize(ctx, SummarizeInput{Path: file, ProjectRoot: root, Mode: "entropy"})
	if err != nil {
		t.Fatalf("Summarize returned a transport error: %v", err)
	}
	if out.Status != "error" {
		t.Fatalf("status = %q, want \"error\" for unrecognized mode", out.Status)
	}
	// entropy isn't a read mode at all (it names dex compress --mode=entropy,
	// #854); the hint should say so, not just "unknown".
	if !strings.Contains(out.Hint, "not a read mode") && !strings.Contains(out.Hint, "unrecognized") {
		t.Errorf("hint = %q, want it to flag the mode as not-a-read-mode or unrecognized", out.Hint)
	}
	// The raw file content must NOT leak through on the error path.
	if strings.Contains(out.Content, "filler line") {
		t.Errorf("error path leaked full file content (token blow-up regression)")
	}
}
