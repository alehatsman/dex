package mcp

import "testing"

func readLook(path string, start, end int, content string) *LookOutput {
	return &LookOutput{
		Result: LookResult{Kind: "read", Read: &SummarizeOutput{
			Path: path, StartLine: start, EndLine: end, Content: content, Status: "ok",
		}},
	}
}

// look's read lane auto-dedups a range already surfaced this session (#110 step
// 3): first sight retains content; a repeat of the same bytes is suppressed with
// SeenTurn pointing at the first turn; changed bytes stay fresh; key="" disables.
func TestApplySeenLook(t *testing.T) {
	s := &Server{}
	const key = "sess-1"

	o1 := readLook("a.go", 10, 20, "line body")
	s.applySeenLook(key, o1)
	if o1.Result.Read.Content == "" || o1.Result.Read.SeenTurn != 0 {
		t.Fatalf("first read must retain content: %+v", o1.Result.Read)
	}

	o2 := readLook("a.go", 10, 20, "line body")
	s.applySeenLook(key, o2)
	if o2.Result.Read.Content != "" {
		t.Fatalf("repeat read must clear content, got %q", o2.Result.Read.Content)
	}
	if o2.Result.Read.SeenTurn != 1 {
		t.Fatalf("SeenTurn must point at the first turn (1), got %d", o2.Result.Read.SeenTurn)
	}
	if o2.Result.Read.Status != "unchanged" || o2.Status != "unchanged" {
		t.Fatalf("suppressed read must read unchanged, got %q/%q", o2.Result.Read.Status, o2.Status)
	}

	o3 := readLook("a.go", 10, 20, "different body")
	s.applySeenLook(key, o3)
	if o3.Result.Read.Content == "" {
		t.Fatal("changed bytes at the same range must not be suppressed")
	}

	o4 := readLook("a.go", 10, 20, "line body")
	s.applySeenLook("", o4)
	if o4.Result.Read.Content == "" {
		t.Fatal("key=\"\" must disable dedup")
	}
}

// Whole-file reads with no line range (start < 1) are not deduped by seen —
// the etag path covers those.
func TestApplySeenLookSkipsWholeFile(t *testing.T) {
	s := &Server{}
	o1 := readLook("a.go", 0, 0, "whole file")
	s.applySeenLook("sess-2", o1)
	o2 := readLook("a.go", 0, 0, "whole file")
	s.applySeenLook("sess-2", o2)
	if o2.Result.Read.Content == "" {
		t.Fatal("range-less whole-file reads must not be seen-suppressed")
	}
}

// The marquee behavior: ask and look share one session ledger and turn counter,
// so a range ask inlined on turn 1 is suppressed when look re-reads the same
// bytes on turn 2 (#110 step 3 — automatic, cross-verb).
func TestSeenLedgerSharedAcrossAskAndLook(t *testing.T) {
	s := &Server{}
	const key = "sess-x"

	ask := &ContextOutput{SuggestedReads: []SuggestedRead{
		{Path: "a.go", StartLine: 10, EndLine: 20, Content: "body"},
	}}
	s.applySeenContext(key, ask) // turn 1

	look := readLook("a.go", 10, 20, "body")
	s.applySeenLook(key, look) // turn 2

	if look.Result.Read.Content != "" || look.Result.Read.SeenTurn != 1 {
		t.Fatalf("look must dedup against ask's turn-1 inline: %+v", look.Result.Read)
	}
}
