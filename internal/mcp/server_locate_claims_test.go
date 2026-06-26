package mcp

import (
	"context"
	"testing"
)

// TestLocateVerifyClaims covers the batch claim-verification mode (#708): a set
// of file:line[:symbol] citations resolved in one locate call via the check
// verb's verifyOneClaim. The locateFixture's greet.go has Greet at line 3 and
// Caller at line 5.
func TestLocateVerifyClaims(t *testing.T) {
	s, root := locateFixture(t)
	_, out, err := s.locate(context.Background(), nil, LocateInput{
		ProjectRoot: root,
		Claims: []ClaimRef{
			{Ref: "greet.go:3", Symbol: "Greet"},   // present at line → ok
			{Ref: "greet.go:5", Symbol: "Greet"},   // exists elsewhere → moved
			{Ref: "greet.go:3", Symbol: "Missing"}, // file ok, symbol absent → gone
			{Ref: "missing.go:1", Symbol: "X"},     // path gone → no_file
			{Ref: "greet.go:3"},                    // no expected symbol → ok + enclosing
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "ok" {
		t.Fatalf("status = %q, want ok (hint: %q)", out.Status, out.Hint)
	}
	if len(out.Results) != 5 {
		t.Fatalf("got %d results, want 5: %+v", len(out.Results), out.Results)
	}
	// Batch mode returns ONLY results — no single-target orientation lanes.
	if out.Symbol != "" || out.Path != "" || len(out.Notes) != 0 {
		t.Errorf("batch mode leaked single-target fields: symbol=%q path=%q notes=%d",
			out.Symbol, out.Path, len(out.Notes))
	}

	r := out.Results
	if r[0].Status != "ok" || r[0].SymbolAt != "Greet" {
		t.Errorf("claim[0] = %+v, want ok/Greet", r[0])
	}
	if r[1].Status != "moved" || r[1].FoundAt != "greet.go:3" {
		t.Errorf("claim[1] = %+v, want moved → greet.go:3", r[1])
	}
	// SymbolAt names what is ACTUALLY at the cited line ("Caller") even when
	// the expected symbol has moved — useful context for the agent.
	if r[1].SymbolAt == "Greet" {
		t.Errorf("claim[1] SymbolAt = %q, want the symbol at line 5 (not Greet)", r[1].SymbolAt)
	}
	if r[2].Status != "gone" {
		t.Errorf("claim[2] = %+v, want gone", r[2])
	}
	if r[3].Status != "no_file" {
		t.Errorf("claim[3] = %+v, want no_file", r[3])
	}
	if r[4].Status != "ok" || r[4].SymbolAt != "Greet" {
		t.Errorf("claim[4] = %+v, want ok with enclosing Greet", r[4])
	}
	// Ref echoes the input verbatim so the agent can map verdict→citation.
	if r[0].Ref != "greet.go:3" {
		t.Errorf("claim[0].Ref = %q, want echo of input", r[0].Ref)
	}
}

// TestLocateClaimsAbsolutePath checks an absolute ref inside the project is
// rebased to a project-relative path before the index lookup.
func TestLocateClaimsAbsolutePath(t *testing.T) {
	s, root := locateFixture(t)
	_, out, err := s.locate(context.Background(), nil, LocateInput{
		ProjectRoot: root,
		Claims:      []ClaimRef{{Ref: root + "/greet.go:3", Symbol: "Greet"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 1 || out.Results[0].Status != "ok" {
		t.Fatalf("absolute-path claim = %+v, want one ok result", out.Results)
	}
}
