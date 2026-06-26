package proxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestColdPrefixTracker_FirstSighting(t *testing.T) {
	dir := t.TempDir()
	tr := &ColdPrefixTracker{path: filepath.Join(dir, "touch.json")}

	// First sighting: no prior touch → ShouldRepack must be false.
	if tr.ShouldRepack() {
		t.Fatal("ShouldRepack true on first sighting; want false")
	}
	tr.Touch()
	// Still false immediately after touch (elapsed ≈ 0).
	if tr.ShouldRepack() {
		t.Fatal("ShouldRepack true immediately after first touch; want false")
	}
}

func TestColdPrefixTracker_ExpiredGap(t *testing.T) {
	dir := t.TempDir()
	tr := &ColdPrefixTracker{path: filepath.Join(dir, "touch.json")}

	// Backdate lastTouch to simulate an expired gap.
	tr.lastTouch = time.Now().Add(-(coldThreshold + time.Second))

	if !tr.ShouldRepack() {
		t.Fatal("ShouldRepack false after cold gap; want true")
	}
}

func TestColdPrefixTracker_LatchIdempotent(t *testing.T) {
	dir := t.TempDir()
	tr := &ColdPrefixTracker{path: filepath.Join(dir, "touch.json")}
	tr.lastTouch = time.Now().Add(-(coldThreshold + time.Second))

	tr.SetRepacking()
	if !tr.IsRepacking() {
		t.Fatal("IsRepacking false after SetRepacking; want true")
	}
	// ShouldRepack returns false once already latched.
	if tr.ShouldRepack() {
		t.Fatal("ShouldRepack true when already repacking; want false")
	}
	// Second SetRepacking is a no-op (no panic, flag stays true).
	tr.SetRepacking()
	if !tr.IsRepacking() {
		t.Fatal("IsRepacking false after second SetRepacking; want true")
	}
}

func TestColdPrefixTracker_PersistAndLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "touch.json")

	tr := &ColdPrefixTracker{path: path}
	tr.lastTouch = time.Now().Add(-(coldThreshold + time.Second))
	tr.SetRepacking()

	// Verify file was written.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("touch file not written: %v", err)
	}
	var st coldTouchState
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("unmarshal touch file: %v", err)
	}
	if !st.Repacking {
		t.Fatal("persisted repacking=false; want true")
	}

	// Load into a fresh tracker and confirm state is restored.
	tr2 := &ColdPrefixTracker{path: path}
	tr2.load()
	if !tr2.IsRepacking() {
		t.Fatal("reloaded tracker IsRepacking=false; want true")
	}
}

func TestColdPrefixTracker_TouchWriteThrottle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "touch.json")

	tr := &ColdPrefixTracker{path: path}
	tr.Touch() // should write (lastWrite is zero)

	stat1, err := os.Stat(path)
	if err != nil {
		t.Fatalf("touch file missing after first Touch: %v", err)
	}

	// Immediate second touch should NOT write (throttled).
	tr.Touch()
	stat2, _ := os.Stat(path)
	if stat2.ModTime() != stat1.ModTime() {
		t.Fatal("touch file rewritten within throttle window; want no write")
	}
}

func TestColdPrefixTracker_FailOpen(t *testing.T) {
	// Empty path → all operations are no-ops, no panics.
	tr := &ColdPrefixTracker{path: ""}
	tr.Touch()
	tr.SetRepacking()
	if !tr.IsRepacking() {
		t.Fatal("IsRepacking false after SetRepacking with empty path; want true")
	}
}
