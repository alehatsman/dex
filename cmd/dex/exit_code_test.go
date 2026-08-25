package main

import "testing"

// checkStatusFailed is the single source of truth for the non-zero exit code of
// `dex check`, consulted before the text/JSON render split so both output modes
// agree. Regression guard for the bug where --format json returned before the
// exit logic and silently reported success on failure.

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
