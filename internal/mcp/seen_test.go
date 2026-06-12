package mcp

import "testing"

func TestSeenMarksRepeatsAcrossTurns(t *testing.T) {
	s := &Server{}
	const key = "sess-1"
	mk := func() *ContextOutput {
		return &ContextOutput{
			SemanticHits:   []SemHit{{Path: "a.go", StartLine: 1, EndLine: 10, Content: "body"}},
			Symbols:        []SymbolHit{{Path: "b.go", StartLine: 5, EndLine: 9, Body: "sym body"}},
			SuggestedReads: []SuggestedRead{{Path: "a.go", StartLine: 1, EndLine: 10, Content: "read"}},
		}
	}

	// Turn 1: nothing seen before; content stays.
	o1 := mk()
	s.applySeenContext(key, o1)
	if o1.SemanticHits[0].SeenTurn != 0 || o1.SemanticHits[0].Content == "" {
		t.Fatalf("turn 1 sem: SeenTurn=%d content=%q, want 0 / kept", o1.SemanticHits[0].SeenTurn, o1.SemanticHits[0].Content)
	}
	// a.go:1-10 appears in both sem and reads this same turn — the second lane
	// must NOT be marked seen (not a cross-turn repeat).
	if o1.SuggestedReads[0].SeenTurn != 0 || o1.SuggestedReads[0].Content == "" {
		t.Fatalf("turn 1 read (same-turn dup) marked seen: SeenTurn=%d", o1.SuggestedReads[0].SeenTurn)
	}

	// Turn 2: same ranges now repeat → marked seen on turn 1, content dropped.
	o2 := mk()
	s.applySeenContext(key, o2)
	if o2.SemanticHits[0].SeenTurn != 1 || o2.SemanticHits[0].Content != "" {
		t.Errorf("turn 2 sem: SeenTurn=%d content=%q, want 1 / empty", o2.SemanticHits[0].SeenTurn, o2.SemanticHits[0].Content)
	}
	if o2.Symbols[0].SeenTurn != 1 || o2.Symbols[0].Body != "" {
		t.Errorf("turn 2 sym: SeenTurn=%d body=%q, want 1 / empty", o2.Symbols[0].SeenTurn, o2.Symbols[0].Body)
	}
	if o2.SuggestedReads[0].SeenTurn != 1 || o2.SuggestedReads[0].Content != "" {
		t.Errorf("turn 2 read: SeenTurn=%d content=%q, want 1 / empty", o2.SuggestedReads[0].SeenTurn, o2.SuggestedReads[0].Content)
	}
}

func TestSeenIsolatesSessions(t *testing.T) {
	s := &Server{}
	mk := func() *ContextOutput {
		return &ContextOutput{SemanticHits: []SemHit{{Path: "a.go", StartLine: 1, EndLine: 10, Content: "body"}}}
	}
	s.applySeenContext("sess-A", mk())
	other := mk()
	s.applySeenContext("sess-B", other) // different session — never seen before
	if other.SemanticHits[0].SeenTurn != 0 || other.SemanticHits[0].Content == "" {
		t.Errorf("session B should not inherit A's seen-set: SeenTurn=%d", other.SemanticHits[0].SeenTurn)
	}
}

func TestSeenDisabledWithoutKey(t *testing.T) {
	s := &Server{}
	mk := func() *ContextOutput {
		return &ContextOutput{SemanticHits: []SemHit{{Path: "a.go", StartLine: 1, EndLine: 10, Content: "body"}}}
	}
	// Empty key (nil request / CLI) disables dedup — repeated calls never mark seen.
	s.applySeenContext("", mk())
	o := mk()
	s.applySeenContext("", o)
	if o.SemanticHits[0].SeenTurn != 0 {
		t.Errorf("empty key must disable dedup, got SeenTurn=%d", o.SemanticHits[0].SeenTurn)
	}
}

func TestSessionKeyNilRequest(t *testing.T) {
	if k := sessionKey(nil); k != "" {
		t.Errorf("sessionKey(nil)=%q, want empty", k)
	}
}
