package mcp

import (
	"testing"
	"time"

	"github.com/alehatsman/dex/internal/chat"
)

func TestBounceTracker_NoBounce(t *testing.T) {
	bt := newBounceTracker()
	// First read of a file that was never compressed — no force-full.
	bt.recordRead("s1", "store.go")
	if bt.shouldForceFull("s1", "store.go") {
		t.Error("expected no force-full on first read of uncompressed file")
	}
}

func TestBounceTracker_BounceDetected(t *testing.T) {
	bt := newBounceTracker()
	// Deliver compressed, then re-read within window.
	bt.recordCompressed("s1", "store.go")
	bt.recordRead("s1", "store.go")
	if !bt.shouldForceFull("s1", "store.go") {
		t.Error("expected force-full after compressed+re-read")
	}
	// Single-use: second shouldForceFull call resets.
	if bt.shouldForceFull("s1", "store.go") {
		t.Error("expected no force-full on second call (single-use)")
	}
}

func TestBounceTracker_WindowExpired(t *testing.T) {
	bt := newBounceTracker()
	// Manually inject a stale compressed entry (past the window).
	k := bounceKey("s1", "store.go")
	bt.mu.Lock()
	bt.compressed[k] = time.Now().Add(-2 * bounceWindow)
	bt.mu.Unlock()

	bt.recordRead("s1", "store.go")
	if bt.shouldForceFull("s1", "store.go") {
		t.Error("expected no force-full when compressed entry is stale")
	}
}

func TestBounceTracker_SessionIsolation(t *testing.T) {
	bt := newBounceTracker()
	bt.recordCompressed("s1", "store.go")
	bt.recordRead("s2", "store.go") // different session
	if bt.shouldForceFull("s2", "store.go") {
		t.Error("bounce should not cross session boundaries")
	}
}

func TestEscalateOnBounce_AnalyzeNeverPromotedToLLM(t *testing.T) {
	// mode=analyze must never be escalated to summary (LLM) on bounce (#752).
	// Wire a chat client so other modes would escalate; verify analyze stays analyze.
	bt := newBounceTracker()
	bt.recordCompressed("s1", "analyze.go")
	bt.recordRead("s1", "analyze.go") // triggers shouldForceFull

	chatSrv := fakeChat(t, "should not be called")
	defer chatSrv.Close()
	srv := newServer("http://127.0.0.1:1", t.TempDir())
	srv.ChatClient = chat.New(chatSrv.URL, "fake", 30*time.Second)

	mode, isLLM := srv.escalateOnBounce(bt, "s1", "analyze.go", ReadModeAnalyze, false)
	if mode != ReadModeAnalyze {
		t.Errorf("want ReadModeAnalyze, got %s", mode)
	}
	if isLLM {
		t.Error("analyze bounce must not set isLLM=true")
	}
}

func TestSelectAffordableMode_NoBudget(t *testing.T) {
	// No budget = no downgrade.
	got := selectAffordableMode(ReadModeFull, 10000, 0)
	if got != ReadModeFull {
		t.Errorf("want full, got %s", got)
	}
}

func TestSelectAffordableMode_Downgrade(t *testing.T) {
	// 10k tokens file with 500 token budget — full won't fit, signatures might.
	// signatures ≈ 10000 * 0.20 = 2000 > 500
	// map ≈ 10000 * 0.12 = 1200 > 500
	// handle = 25 ≤ 500
	got := selectAffordableMode(ReadModeFull, 10000, 500)
	if got != ReadModeHandle {
		t.Errorf("want handle, got %s", got)
	}
}

func TestSelectAffordableMode_SkeletonFits(t *testing.T) {
	// 1000 token file with 300 budget: skeleton ≈ 300 ≤ 300 (fits before signatures).
	got := selectAffordableMode(ReadModeFull, 1000, 300)
	if got != ReadModeSkeleton {
		t.Errorf("want skeleton, got %s", got)
	}
}

func TestSelectAffordableMode_SignaturesFits(t *testing.T) {
	// 1000 token file with 250 budget: skeleton ≈ 300 > 250; signatures ≈ 200 ≤ 250.
	got := selectAffordableMode(ReadModeFull, 1000, 250)
	if got != ReadModeSignatures {
		t.Errorf("want signatures, got %s", got)
	}
}

func TestViewDowngradeChain(t *testing.T) {
	chain := viewDowngradeChain(ReadModeFull)
	if chain[0] != ReadModeFull || chain[len(chain)-1] != ReadModeHandle {
		t.Errorf("downgrade chain %v should start with full and end with handle", chain)
	}
}
