package main

import (
	"testing"
)

// read / boundary build the consume + session-boundary events the
// --mine-curated tests join against. The open-rate join itself is tested in
// internal/feedback; here we exercise the CLI-only miss-miner.
func read(path string) feedbackEvent {
	return feedbackEvent{ToolName: "Read", Paths: []string{path}}
}
func boundary() feedbackEvent { return feedbackEvent{Event: "Stop"} }

func ask(intent string, _ int, paths ...string) feedbackEvent {
	return feedbackEvent{ToolName: "mcp__dex__ask", Intent: intent, Paths: paths}
}

func askQ(query, intent string, paths ...string) feedbackEvent {
	return feedbackEvent{ToolName: "mcp__dex__ask", Intent: intent, Query: query, Paths: paths}
}

func TestMineCurated_BasicMiss(t *testing.T) {
	// ask recommends a.go; agent opens b.go (not suggested) → candidate
	events := []feedbackEvent{
		askQ("how does chunking work", "behavior_search", "a.go"),
		read("b.go"),
	}
	got := mineCuratedCandidates(events, 0, "/proj")
	if len(got) != 1 {
		t.Fatalf("want 1 candidate, got %d", len(got))
	}
	if got[0].Query != "how does chunking work" {
		t.Errorf("wrong query: %q", got[0].Query)
	}
	if len(got[0].RelevantFiles) != 1 || got[0].RelevantFiles[0] != "b.go" {
		t.Errorf("wrong relevant_files: %v", got[0].RelevantFiles)
	}
}

func TestMineCurated_NoMissWhenSuggested(t *testing.T) {
	// agent opens exactly what was suggested → no candidate
	events := []feedbackEvent{
		askQ("how does indexing work", "behavior_search", "index.go"),
		read("index.go"),
	}
	if got := mineCuratedCandidates(events, 0, "/proj"); len(got) != 0 {
		t.Errorf("want 0 candidates, got %d", len(got))
	}
}

func TestMineCurated_EmptyQuerySkipped(t *testing.T) {
	// ask event with no query field must be skipped
	events := []feedbackEvent{
		ask("auto", 0, "a.go"), // no Query
		read("b.go"),
	}
	if got := mineCuratedCandidates(events, 0, "/proj"); len(got) != 0 {
		t.Errorf("want 0 candidates for empty query, got %d", len(got))
	}
}

func TestMineCurated_CrossSessionMerge(t *testing.T) {
	// same query misses b.go in 2 sessions → session_count=2, single candidate
	events := []feedbackEvent{
		askQ("how does chunking work", "auto", "a.go"),
		read("b.go"),
		boundary(),
		askQ("how does chunking work", "auto", "a.go"),
		read("b.go"),
	}
	got := mineCuratedCandidates(events, 0, "/proj")
	if len(got) != 1 {
		t.Fatalf("want 1 merged candidate, got %d", len(got))
	}
	// Verify the ID is stable (deterministic from query).
	if got[0].ID != queryID("how does chunking work") {
		t.Errorf("ID mismatch: %q", got[0].ID)
	}
}

func TestMineCurated_SessionScoped(t *testing.T) {
	// file opened in a later session must not be credited to an earlier ask
	events := []feedbackEvent{
		askQ("how does chunking work", "auto", "a.go"),
		boundary(),
		read("b.go"), // different session
	}
	if got := mineCuratedCandidates(events, 0, "/proj"); len(got) != 0 {
		t.Errorf("cross-session open must not produce a candidate, got %d", len(got))
	}
}

func TestMineCurated_AbsolutePathMadeRelative(t *testing.T) {
	// absolute opened path should be made relative to project root
	events := []feedbackEvent{
		askQ("where is the lock", "symbol_lookup", "internal/lock/lock.go"),
		read("/proj/internal/proxy/prune.go"),
	}
	got := mineCuratedCandidates(events, 0, "/proj")
	if len(got) != 1 {
		t.Fatalf("want 1 candidate, got %d", len(got))
	}
	if got[0].RelevantFiles[0] != "internal/proxy/prune.go" {
		t.Errorf("path not made relative: %q", got[0].RelevantFiles[0])
	}
}
