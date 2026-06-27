package feedback

import (
	"math"
	"testing"
)

func ask(intent string, inlined int, paths ...string) Event {
	return Event{ToolName: "mcp__dex__ask", Intent: intent, Inlined: inlined, Paths: paths}
}
func read(path string) Event { return Event{ToolName: "Read", Paths: []string{path}} }
func boundary() Event        { return Event{Event: "Stop"} }

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestComputeOpenRate(t *testing.T) {
	// ask recommends a.go + b.go; agent opens only a.go.
	events := []Event{
		ask("assemble", 500, "a.go", "b.go"),
		read("a.go"),
	}
	r := Compute(events, 0)
	if r.SuggestedReads != 2 || r.OpenedReads != 1 {
		t.Fatalf("opened %d/%d, want 1/2", r.OpenedReads, r.SuggestedReads)
	}
	if !approx(r.OpenRate, 0.5) {
		t.Errorf("open_rate = %v, want 0.5", r.OpenRate)
	}
	if r.InlinedAsks != 1 || r.InlineReopened != 1 {
		t.Errorf("inline-reopen %d/%d, want 1/1", r.InlineReopened, r.InlinedAsks)
	}
	if st := r.ByIntent["assemble"]; st.Asks != 1 || st.Opened != 1 {
		t.Errorf("by-intent assemble = %+v", st)
	}
}

// The join must not cross a session boundary.
func TestComputeSessionScoped(t *testing.T) {
	events := []Event{
		ask("behavior_search", 0, "x.go"),
		boundary(),
		read("x.go"), // different session — must NOT count
	}
	r := Compute(events, 0)
	if r.Sessions != 2 {
		t.Errorf("sessions = %d, want 2", r.Sessions)
	}
	if r.OpenedReads != 0 {
		t.Errorf("cross-session read was credited: opened=%d", r.OpenedReads)
	}
}

func TestComputeWindow(t *testing.T) {
	events := []Event{
		ask("auto", 0, "a.go"),
		read("noise1.go"),
		read("noise2.go"),
		read("a.go"),
	}
	if r := Compute(events, 2); r.OpenedReads != 0 {
		t.Errorf("window=2 should miss the 3rd-consume open, got %d", r.OpenedReads)
	}
	if r := Compute(events, 0); r.OpenedReads != 1 {
		t.Errorf("window=0 (unbounded) should find the open, got %d", r.OpenedReads)
	}
}

func TestPathMatch(t *testing.T) {
	if !PathMatch("internal/proxy/prune.go", "/home/aleh/projects/dex/internal/proxy/prune.go") {
		t.Error("relative suggested vs absolute opened should match")
	}
	if !PathMatch("a/b.go", "a/b.go") {
		t.Error("identical paths should match")
	}
	if PathMatch("internal/proxy/prune.go", "internal/proxy/ccr.go") {
		t.Error("different files must not match")
	}
	// guard against bare-suffix false positive (not a component boundary).
	if PathMatch("rune.go", "/x/prune.go") {
		t.Error("non-component-boundary suffix must not match")
	}
}

func TestIsAskAndConsumeTools(t *testing.T) {
	if !IsAskTool("mcp__dex__ask") || !IsAskTool("ask") {
		t.Error("ask tools not recognized")
	}
	if IsAskTool("Read") || IsAskTool("mcp__dex__find") {
		t.Error("non-ask tool misclassified as ask")
	}
	for _, c := range []string{"Read", "Edit", "Write", "NotebookEdit", "mcp__dex__read"} {
		if !IsConsumeTool(c) {
			t.Errorf("%q should be a consume tool", c)
		}
	}
	if IsConsumeTool("Bash") || IsConsumeTool("mcp__dex__ask") {
		t.Error("non-consume tool misclassified")
	}
}
