package mcp

import (
	"encoding/base64"
	"testing"
)

func TestHandleRoundTrip(t *testing.T) {
	cases := []struct {
		path       string
		start, end int
	}{
		{"internal/mcp/server.go", 1, 12},
		{"a.go", 1, 1},
		{"deeply/nested/pkg/file_test.go", 100, 250},
		{"file with spaces.go", 5, 9}, // path may contain spaces
		{"weird\tname.go", 5, 9},      // a tab in the path must survive (it's the field sep)
		{"unicode/файл.go", 3, 4},     // non-ASCII path bytes
	}
	for _, c := range cases {
		h := EncodeHandle(c.path, c.start, c.end)
		path, start, end, ok := DecodeHandle(h)
		if !ok {
			t.Fatalf("DecodeHandle(%q) for %q: ok=false, want true", h, c.path)
		}
		if path != c.path || start != c.start || end != c.end {
			t.Errorf("round-trip %q:%d-%d -> %q:%d-%d", c.path, c.start, c.end, path, start, end)
		}
	}
}

func TestEncodeHandleClamps(t *testing.T) {
	// start < 1 clamps to 1; end < start clamps to start.
	h := EncodeHandle("x.go", 0, 0)
	_, start, end, ok := DecodeHandle(h)
	if !ok || start != 1 || end != 1 {
		t.Errorf("clamp(0,0) -> ok=%v start=%d end=%d, want ok=true 1 1", ok, start, end)
	}
	h = EncodeHandle("x.go", 10, 3)
	_, start, end, _ = DecodeHandle(h)
	if start != 10 || end != 10 {
		t.Errorf("clamp(10,3) -> start=%d end=%d, want 10 10", start, end)
	}
}

func TestDecodeHandleRejectsGarbage(t *testing.T) {
	// Payloads are start\tend\tpath (line numbers first, path last).
	bad := []string{
		"",                       // empty
		"not base64 !@#$",        // invalid base64url
		b64("5"),                 // wrong field count (1)
		b64("5\t9"),              // wrong field count (2)
		b64("nope\t9\tx.go"),     // unparseable start
		b64("5\tnope\tx.go"),     // unparseable end
		b64("5\t3\tx.go"),        // end < start
		b64("0\t5\tx.go"),        // start < 1
		b64("5\t9\t"),            // empty path
		b64("1\t2\t/etc/passwd"), // absolute path
		b64("1\t2\t../../etc/x"), // traversal
		b64("1\t2\ta/../b.go"),   // embedded traversal
		b64("1\t2\ta/./b.go"),    // embedded "." segment
		b64("1\t2\t~/secrets"),   // home expansion
		b64("1\t2\tC:\\win\\x"),  // windows drive + backslash
		b64("1\t2\ta//b.go"),     // empty segment
	}
	for _, h := range bad {
		if _, _, _, ok := DecodeHandle(h); ok {
			t.Errorf("DecodeHandle(%q): ok=true, want false (garbage rejected)", h)
		}
	}
}

func TestValidateHandlePath(t *testing.T) {
	good := []string{"a.go", "a/b/c.go", "deep/nested/path.rs", "файл.go"}
	for _, p := range good {
		if !validateHandlePath(p) {
			t.Errorf("validateHandlePath(%q)=false, want true", p)
		}
	}
	bad := []string{"", "/abs", "../up", "a/../b", "a/./b", "~/x", "C:/x", "a\\b", "a//b"}
	for _, p := range bad {
		if validateHandlePath(p) {
			t.Errorf("validateHandlePath(%q)=true, want false", p)
		}
	}
}

// b64 encodes a raw payload the way EncodeHandle does, so tests can hand-craft
// malformed payloads the public Encode path would never mint.
func b64(payload string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(payload))
}
