package mcp

import (
	"testing"
)

func TestDiaryAppendAndRecall(t *testing.T) {
	dir := t.TempDir()

	// Append two entries.
	e1, err := DiaryAppend(dir, "agent-1", "discovery", "rate limiting is in middleware/rl.go")
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if e1.ID != 1 {
		t.Errorf("first entry id = %d, want 1", e1.ID)
	}
	if e1.Category != "discovery" {
		t.Errorf("category = %q, want discovery", e1.Category)
	}

	e2, err := DiaryAppend(dir, "agent-1", "decision", "use token bucket, not leaky bucket")
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if e2.ID != 2 {
		t.Errorf("second entry id = %d, want 2", e2.ID)
	}

	// recall_diary returns newest-first.
	entries, err := DiaryRecall(dir, "agent-1", 10)
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0].ID != 2 {
		t.Errorf("newest-first: got id %d, want 2", entries[0].ID)
	}
	if entries[1].ID != 1 {
		t.Errorf("oldest last: got id %d, want 1", entries[1].ID)
	}
}

func TestDiaryRecallMissingAgent(t *testing.T) {
	dir := t.TempDir()
	entries, err := DiaryRecall(dir, "nonexistent", 10)
	if err != nil {
		t.Fatalf("recall on missing agent: %v", err)
	}
	if entries != nil {
		t.Errorf("expected nil entries, got %v", entries)
	}
}

func TestDiaryCap(t *testing.T) {
	dir := t.TempDir()

	// Append 110 entries — should be capped at 100.
	for i := range 110 {
		_, err := DiaryAppend(dir, "agent-cap", "progress", "step")
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	entries, err := DiaryRecall(dir, "agent-cap", 200)
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(entries) != diaryMaxEntries {
		t.Errorf("expected %d entries after cap, got %d", diaryMaxEntries, len(entries))
	}
	// Newest entry should have the highest id.
	if entries[0].ID != 110 {
		t.Errorf("newest id = %d, want 110", entries[0].ID)
	}
	// Oldest retained is entry 11 (110 - 100 + 1).
	if entries[diaryMaxEntries-1].ID != 11 {
		t.Errorf("oldest retained id = %d, want 11", entries[diaryMaxEntries-1].ID)
	}
}

func TestDiaryDefaultCategory(t *testing.T) {
	dir := t.TempDir()
	e, err := DiaryAppend(dir, "agent-1", "", "something noted")
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	// Default category is applied by the handler, not DiaryAppend — so empty is stored as-is here.
	// This test just verifies empty category doesn't blow up.
	if e.Content != "something noted" {
		t.Errorf("content = %q, want 'something noted'", e.Content)
	}
}

func TestDiaryList(t *testing.T) {
	dir := t.TempDir()

	_, _ = DiaryAppend(dir, "alpha", "discovery", "found X")
	_, _ = DiaryAppend(dir, "beta", "decision", "chose Y")
	_, _ = DiaryAppend(dir, "beta", "progress", "done Z")

	list, err := DiaryList(dir)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 agents, got %d", len(list))
	}

	counts := map[string]int{}
	for _, row := range list {
		counts[row.AgentID] = row.EntryCount
	}
	if counts["alpha"] != 1 {
		t.Errorf("alpha count = %d, want 1", counts["alpha"])
	}
	if counts["beta"] != 2 {
		t.Errorf("beta count = %d, want 2", counts["beta"])
	}
}

func TestDiaryListEmpty(t *testing.T) {
	dir := t.TempDir()
	list, err := DiaryList(dir)
	if err != nil {
		t.Fatalf("list on empty: %v", err)
	}
	if list != nil {
		t.Errorf("expected nil list, got %v", list)
	}
}
