package throttle

import (
	"strings"
	"testing"
	"time"
)

func TestDetector_Normal(t *testing.T) {
	d := New()
	level, hint := d.Check("search_semantic", "foo", false)
	if level != Normal {
		t.Fatalf("first call: want Normal, got %v", level)
	}
	if hint != "" {
		t.Fatalf("first call: want no hint, got %q", hint)
	}
}

func TestDetector_HintThreshold(t *testing.T) {
	d := New()
	var level Level
	var hint string
	for i := 0; i < hintThreshold; i++ {
		level, hint = d.Check("ask", "same query", false)
	}
	if level != Hint {
		t.Fatalf("at hint threshold: want Hint, got %v", level)
	}
	if !strings.Contains(hint, "repeated") {
		t.Fatalf("hint should mention 'repeated', got %q", hint)
	}
}

func TestDetector_ReduceThreshold(t *testing.T) {
	d := New()
	var level Level
	for i := 0; i < reduceThreshold; i++ {
		level, _ = d.Check("search_symbol", "FooBar", false)
	}
	if level != Reduce {
		t.Fatalf("at reduce threshold: want Reduce, got %v", level)
	}
}

func TestDetector_BlockThreshold(t *testing.T) {
	d := New()
	var level Level
	var hint string
	for i := 0; i < blockThreshold; i++ {
		level, hint = d.Check("search_grep", "pattern.*", false)
	}
	if level != Block {
		t.Fatalf("at block threshold: want Block, got %v", level)
	}
	if !strings.Contains(hint, "loop-blocked") {
		t.Fatalf("blocked hint should contain 'loop-blocked', got %q", hint)
	}
}

func TestDetector_SearchGroupLimit(t *testing.T) {
	d := New()
	var level Level
	var hint string
	// Each call with a distinct fingerprint so only the group limit fires.
	for i := 0; i < searchGroupLimit; i++ {
		level, hint = d.Check("search_semantic", Fingerprint("unique", string(rune('a'+i))), true)
	}
	if level != Block {
		t.Fatalf("at search group limit: want Block, got %v (hint: %q)", level, hint)
	}
	if !strings.Contains(hint, "search-group-blocked") {
		t.Fatalf("group-blocked hint missing, got %q", hint)
	}
}

func TestDetector_DifferentToolsDontShare(t *testing.T) {
	d := New()
	// Same args, different tools — should not accumulate toward the same counter.
	for i := 0; i < blockThreshold-1; i++ {
		d.Check("search_semantic", "q", false)
	}
	level, _ := d.Check("ask", "q", false)
	// ask has only seen the call once, should be Normal or Hint.
	if level == Block {
		t.Fatal("different tool should not be blocked by other tool's count")
	}
}

func TestDetector_WindowExpiry(t *testing.T) {
	d := New()
	// Fill up to block threshold.
	for i := 0; i < blockThreshold; i++ {
		d.Check("search_semantic", "expiry-test", false)
	}
	// Manually age all timestamps past the window.
	d.mu.Lock()
	fp := Fingerprint("search_semantic", "expiry-test")
	if e, ok := d.calls[fp]; ok {
		old := time.Now().Add(-(window + time.Second))
		for i := range e.timestamps {
			e.timestamps[i] = old
		}
	}
	d.mu.Unlock()

	level, _ := d.Check("search_semantic", "expiry-test", false)
	if level == Block {
		t.Fatal("after window expiry, call should not be blocked")
	}
}
