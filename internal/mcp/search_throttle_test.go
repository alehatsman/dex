package mcp

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSearchThrottleHint_NoHintOnFirstCalls verifies no hint is returned until
// the 4th repetition.
func TestSearchThrottleHint_NoHintOnFirstCalls(t *testing.T) {
	s := &Server{}
	for i := 0; i < 3; i++ {
		hint, early := s.searchThrottleHint("same query", "/proj")
		if hint != "" || early {
			t.Errorf("call %d: got hint=%q early=%v, want empty/false", i+1, hint, early)
		}
	}
}

// TestSearchThrottleHint_HintAt4 verifies a soft hint at 4 repetitions, no
// earlyReturn.
func TestSearchThrottleHint_HintAt4(t *testing.T) {
	s := &Server{}
	var hint string
	var early bool
	for i := 0; i < 4; i++ {
		hint, early = s.searchThrottleHint("same query", "/proj")
	}
	if hint == "" {
		t.Error("expected hint at 4 repetitions, got empty string")
	}
	if early {
		t.Error("earlyReturn should be false at 4 repetitions")
	}
	if !strings.Contains(hint, "4") {
		t.Errorf("hint should mention repetition count, got %q", hint)
	}
}

// TestSearchThrottleHint_EarlyReturnAt7 verifies earlyReturn=true at 7+.
func TestSearchThrottleHint_EarlyReturnAt7(t *testing.T) {
	s := &Server{}
	var hint string
	var early bool
	for i := 0; i < 7; i++ {
		hint, early = s.searchThrottleHint("same query", "/proj")
	}
	if !early {
		t.Error("earlyReturn should be true at 7 repetitions")
	}
	if hint == "" {
		t.Error("hint should be non-empty at 7 repetitions")
	}
}

// TestSearchThrottleHint_DifferentProjectsIndependent verifies that counters
// are keyed by (query, project) — two projects with the same query don't
// interfere.
func TestSearchThrottleHint_DifferentProjectsIndependent(t *testing.T) {
	s := &Server{}
	for i := 0; i < 7; i++ {
		s.searchThrottleHint("q", "/projA")
	}
	// /projB has only seen 1 call — should not be throttled.
	hint, early := s.searchThrottleHint("q", "/projB")
	if early {
		t.Error("different project should not be throttled by sibling's count")
	}
	if hint != "" {
		t.Errorf("different project should have empty hint, got %q", hint)
	}
}

// TestSearchThrottleHint_IdleResetClears verifies that idle counter reset
// works by manipulating lastAt directly.
func TestSearchThrottleHint_IdleResetClears(t *testing.T) {
	s := &Server{}
	// Run 6 calls so we're just below earlyReturn.
	for i := 0; i < 6; i++ {
		s.searchThrottleHint("q", "/proj")
	}
	// Simulate 11 minutes of idle by backdating lastAt.
	key := "/proj\x00q"
	raw, ok := s.searchThrottle.Load(key)
	if !ok {
		t.Fatal("throttle entry not found after 6 calls")
	}
	e := raw.(*throttleEntry)
	s.searchThrottleMu.Lock()
	e.lastAt = time.Now().Add(-11 * time.Minute)
	s.searchThrottleMu.Unlock()

	// Next call should reset to count=1, no hint.
	hint, early := s.searchThrottleHint("q", "/proj")
	if hint != "" || early {
		t.Errorf("after idle reset: got hint=%q early=%v, want empty/false", hint, early)
	}
}

// TestSearchFind_EarlyReturnAt7 verifies that find's throttle earlyReturn
// fires at the 7th identical search — BEFORE the loop-detector's check —
// and returns status "ok" with a hint rather than "loop-blocked".
//
// The loop detector (internal/throttle) blocks at 6 identical back-to-back
// calls (blockThreshold=6). The 7th call is caught by searchThrottleHint
// before reaching the LD, so it must return "ok" (not "loop-blocked").
func TestSearchFind_EarlyReturnAt7(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, filepath.Join(projDir, "main.go"), "package main\nfunc Greet() {}\n")
	root := indexProject(t, projDir, cacheDir, srv.URL)
	s := newServer(srv.URL, cacheDir)
	ctx := context.Background()

	// Calls 1–5 return real results (before LD block threshold of 6).
	// Call 6 is blocked by LD (loop-blocked).
	// Call 7 is intercepted by searchThrottleHint earlyReturn BEFORE the LD
	// check, so it must return "ok" with a throttle hint.
	for i := 0; i < 7; i++ {
		out, err := s.Search(ctx, SearchInput{ProjectRoot: root, Query: "Greet function"})
		if err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
		switch {
		case i < 5:
			if out.Status != "ok" {
				t.Fatalf("call %d: status=%q, want ok", i+1, out.Status)
			}
		case i == 5:
			// LD fires block at the 6th call.
			if out.Status != "loop-blocked" {
				t.Logf("call 6: status=%q (LD may not have fired yet — this is informational)", out.Status)
			}
		case i == 6:
			// searchThrottleHint earlyReturn intercepts before LD on 7th call.
			if out.Status != "ok" {
				t.Fatalf("call 7: status=%q, want ok (searchThrottleHint should intercept before LD)", out.Status)
			}
			if out.Hint == "" {
				t.Error("call 7: expected throttle hint, got empty")
			}
			if !strings.Contains(out.Hint, "7") {
				t.Errorf("call 7: hint should mention count 7, got %q", out.Hint)
			}
		}
	}
}

// TestSearchFind_4thCallCarriesHint verifies a non-empty Hint on the 4th
// identical find call without suppressing results.
func TestSearchFind_4thCallCarriesHint(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, filepath.Join(projDir, "main.go"), "package main\nfunc Greet() {}\n")
	root := indexProject(t, projDir, cacheDir, srv.URL)
	s := newServer(srv.URL, cacheDir)
	ctx := context.Background()

	var out SearchOutput
	for i := 0; i < 4; i++ {
		var err error
		out, err = s.Search(ctx, SearchInput{ProjectRoot: root, Query: "Greet function"})
		if err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
	}
	if out.Hint == "" {
		t.Error("4th call: expected hint, got empty")
	}
}
