package mcp

import (
	"context"
	"testing"
)

// TestSummarizeBadHandle: a token that doesn't decode is rejected before any
// filesystem access — the anti-hallucination guarantee. No project/index needed
// because the guard returns first.
func TestSummarizeBadHandle(t *testing.T) {
	s := &Server{}
	for _, bad := range []string{"!!!not-base64!!!", "", "deadbeef"} {
		if bad == "" {
			continue // empty handle means "no handle"; not a bad-handle case
		}
		_, out, err := s.summarize(context.Background(), nil, SummarizeInput{Handle: bad})
		if err != nil {
			t.Fatalf("summarize(%q): unexpected err %v", bad, err)
		}
		if out.Status != "bad-handle" {
			t.Errorf("summarize(handle=%q): status=%q, want bad-handle", bad, out.Status)
		}
	}
}

// TestSummarizeResolvesHandle: a well-formed handle minted for a real file
// resolves to that path and reads it — proving the handle drives path resolution
// end-to-end. lines mode avoids needing a chat backend.
func TestSummarizeResolvesHandle(t *testing.T) {
	projDir := t.TempDir()
	buildGoFile(t, projDir, "a.go", 10, "")

	s := &Server{IndexDir: t.TempDir()}
	h := EncodeHandle("a.go", 1, 50)
	_, out, err := s.summarize(context.Background(), nil, SummarizeInput{
		Handle:      h,
		ProjectRoot: projDir,
		Mode:        "lines:1-50",
	})
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if out.Status != "ok" {
		t.Fatalf("status=%q hint=%s", out.Status, out.Hint)
	}
	if out.Path != "a.go" {
		t.Errorf("Path=%q, want a.go (resolved from handle)", out.Path)
	}
}
