package mcp

import "testing"

func TestStampSearchHandles(t *testing.T) {
	hits := []SearchHit{
		{Path: "internal/mcp/server.go", StartLine: 10, EndLine: 42}, // real range -> handle
		{Path: "git:d6333a7d", StartLine: 0, EndLine: 0},             // commit pseudo-hit -> none
		{Path: "../escape.go", StartLine: 1, EndLine: 5},             // traversal -> none
	}
	stampSearchHandles(hits)

	if hits[0].Handle == "" {
		t.Fatal("real-file hit got no handle")
	}
	path, start, end, ok := DecodeHandle(hits[0].Handle)
	if !ok || path != "internal/mcp/server.go" || start != 10 || end != 42 {
		t.Errorf("stamped handle decodes to %q:%d-%d ok=%v, want internal/mcp/server.go:10-42", path, start, end, ok)
	}
	if hits[1].Handle != "" {
		t.Errorf("commit pseudo-hit (start_line 0) got handle %q, want empty", hits[1].Handle)
	}
	if hits[2].Handle != "" {
		t.Errorf("traversal path got handle %q, want empty", hits[2].Handle)
	}
}

func TestMakeHandleGuards(t *testing.T) {
	if h := makeHandle("a/b.go", 5, 9); h == "" {
		t.Error("valid locator got empty handle")
	}
	if h := makeHandle("a/b.go", 0, 9); h != "" {
		t.Error("start_line < 1 should yield no handle")
	}
	if h := makeHandle("/abs/path.go", 1, 2); h != "" {
		t.Error("absolute path should yield no handle")
	}
}
