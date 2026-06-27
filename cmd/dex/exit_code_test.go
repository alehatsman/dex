package main

import "testing"

// These predicates are the single source of truth for the non-zero exit code
// of `dex check` / `dex verify`, consulted before the text/JSON render split so
// both output modes agree. Regression guard for the bug where --format json
// returned before the exit logic and silently reported success on failure.

func TestCheckStatusFailed(t *testing.T) {
	cases := []struct {
		status string
		want   bool
	}{
		{"ok", false},
		{"moved", true},
		{"gone", true},
		{"no_file", true},
		{"parse_error", true},
		{"", false},
		{"unknown_future_status", false},
	}
	for _, c := range cases {
		if got := checkStatusFailed(c.status); got != c.want {
			t.Errorf("checkStatusFailed(%q) = %v, want %v", c.status, got, c.want)
		}
	}
}

func TestVerifyFailed(t *testing.T) {
	cases := []struct {
		status string
		passed bool
		want   bool
	}{
		{"ok", true, false},     // ran and passed
		{"ok", false, true},     // ran and failed -> non-zero exit
		{"error", false, false}, // could not run -> advisory, exit 0
		{"error", true, false},
		{"", false, false},
	}
	for _, c := range cases {
		if got := verifyFailed(c.status, c.passed); got != c.want {
			t.Errorf("verifyFailed(%q, %v) = %v, want %v", c.status, c.passed, got, c.want)
		}
	}
}
