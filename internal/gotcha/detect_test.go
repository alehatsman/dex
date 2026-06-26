package gotcha

import (
	"strings"
	"testing"
)

func TestDetectSucceedsAreNil(t *testing.T) {
	if c := Detect("go build ./...", "ok", 0); c != nil {
		t.Errorf("exit 0 must not stage a candidate, got %+v", c)
	}
}

func TestDetectUnrecognizedFailureIsNil(t *testing.T) {
	// Non-zero exit but no known signature in the output → nothing to stage.
	if c := Detect("weirdtool", "the moon is full tonight\n", 1); c != nil {
		t.Errorf("unrecognized failure must not stage a candidate, got %+v", c)
	}
}

func TestDetectClassifies(t *testing.T) {
	cases := []struct {
		name   string
		cmd    string
		output string
		want   string // expected Class
	}{
		{"go missing module", "go build ./...", "main.go:3:8: no required module provides package foo", "build"},
		{"undefined symbol", "go build ./...", "./x.go:5:2: undefined: Bar", "build"},
		{"go test fail", "go test ./...", "--- FAIL: TestThing (0.00s)\nFAIL", "test"},
		{"panic", "go run .", "panic: runtime error: index out of range [3]", "panic"},
		{"command not found", "frobnicate", "bash: frobnicate: command not found", "missing-command"},
		{"permission", "cat /etc/shadow", "cat: /etc/shadow: Permission denied", "permission"},
		{"network", "curl x", "curl: (7) Failed to connect: Connection refused", "network"},
		{"auth", "git push", "remote: Permission to org/repo denied to user.", "auth"},
		{"disk", "dd ...", "dd: writing: No space left on device", "disk"},
		{"rust unresolved", "cargo build", "error[E0432]: unresolved import `foo::bar`", "build"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Detect(tc.cmd, tc.output, 1)
			if c == nil {
				t.Fatalf("expected a candidate for %q", tc.output)
			}
			if c.Class != tc.want {
				t.Errorf("Class = %q, want %q (trigger %q)", c.Class, tc.want, c.Trigger)
			}
			if c.Archetype != "Gotcha" {
				t.Errorf("Archetype = %q, want Gotcha", c.Archetype)
			}
			if c.Confidence <= 0 || c.Confidence >= 1 {
				t.Errorf("Confidence = %v, want low (0,1)", c.Confidence)
			}
			if !strings.Contains(c.Suggest, "notes") || !strings.Contains(c.Suggest, "Gotcha") {
				t.Errorf("Suggest should be a runnable notes-add hint, got %q", c.Suggest)
			}
		})
	}
}

// TestDetectScansTail ensures the signal is found even when it sits after a
// large preamble (we scan the tail of the output).
func TestDetectScansTail(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 200; i++ {
		b.WriteString("compiling module ...\n")
	}
	b.WriteString("ld: symbol(s) not found for architecture arm64\n")
	c := Detect("make", b.String(), 2)
	if c == nil || c.Class != "build" {
		t.Fatalf("tail signature should classify as build, got %+v", c)
	}
}

// TestDetectFragmentTruncated guards the output-fragment cap so a giant matched
// line can't bloat the response.
func TestDetectFragmentTruncated(t *testing.T) {
	long := "undefined: " + strings.Repeat("X", 500)
	c := Detect("go build", long, 1)
	if c == nil {
		t.Fatal("expected a candidate")
	}
	if len([]rune(c.OutputFragment)) > 201 { // 200 + ellipsis
		t.Errorf("fragment not truncated: %d runes", len([]rune(c.OutputFragment)))
	}
}
