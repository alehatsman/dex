package compress

import (
	"strings"
	"testing"
)

// #464: severity is signal. Two lines that differ only by log level must NOT
// dedup together, so an ERROR line is never dropped against an earlier
// INFO/DEBUG line with the same message text.
func TestDedupBlockLinesKeepsDistinctSeverities(t *testing.T) {
	in := []string{
		"INFO  connection to db established",
		"ERROR connection to db established", // same text, higher severity — must survive
	}
	out := DedupBlockLines(in)

	joined := strings.Join(out, "\n")
	if !strings.Contains(joined, "ERROR connection to db established") {
		t.Errorf("dedup dropped the ERROR line\n--- got ---\n%s", joined)
	}
	if len(out) != 2 {
		t.Errorf("expected both severities retained (2 lines), got %d: %v", len(out), out)
	}
}

// Genuine repetition (identical text, same level) still collapses — the TS/UUID
// normalization carries the dedup, not level-stripping.
func TestDedupBlockLinesCollapsesTrueDuplicates(t *testing.T) {
	in := []string{
		"2026-06-13T10:00:00Z INFO retrying fetch",
		"2026-06-13T10:00:01Z INFO retrying fetch",
		"2026-06-13T10:00:02Z INFO retrying fetch",
	}
	out := DedupBlockLines(in)
	if len(out) != 1 {
		t.Errorf("expected timestamp-only-difference lines to collapse to 1, got %d: %v", len(out), out)
	}
}
