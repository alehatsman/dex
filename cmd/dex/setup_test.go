package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuildRulesContent covers the writer's decision matrix. The key
// regression (issue #350): a drifted file whose marker/version are present but
// whose body differs from canonical must be rewritten ("updated"), not skipped
// as "already up to date". The old code keyed off rulesVersion presence and
// wedged such files — the doctor flagged drift while `dex setup` refused to fix
// it.
func TestBuildRulesContent(t *testing.T) {
	t.Run("missing file is created", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "dex.md")
		action, content, err := buildRulesContent(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if action != "created" {
			t.Fatalf("action = %q, want created", action)
		}
		if !strings.Contains(content, rulesContent) {
			t.Fatal("created content missing canonical block")
		}
	})

	t.Run("canonical file is left alone", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "dex.md")
		if err := os.WriteFile(path, []byte(rulesContent+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		action, _, err := buildRulesContent(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if action != "already up to date" {
			t.Fatalf("action = %q, want already up to date", action)
		}
	})

	t.Run("drifted block is updated even with version marker present", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "dex.md")
		// Same markers + version string as canonical, but mutated body — exactly
		// the stuck state from #350.
		drifted := strings.Replace(rulesContent, "MCP server instructions", "MCP server instruction", 1)
		if drifted == rulesContent {
			t.Fatal("test setup failed: canonical text changed, fixture no longer drifts")
		}
		if !strings.Contains(drifted, rulesVersion) {
			t.Fatal("test setup failed: fixture lost the version marker")
		}
		if err := os.WriteFile(path, []byte(drifted+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		action, content, err := buildRulesContent(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if action != "updated" {
			t.Fatalf("action = %q, want updated", action)
		}
		if extractRulesBlock(content) != rulesContent {
			t.Fatal("updated content does not match canonical block")
		}
	})

	t.Run("file without markers gets the block appended", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "dex.md")
		preamble := "# my own notes\nkeep me\n"
		if err := os.WriteFile(path, []byte(preamble), 0o644); err != nil {
			t.Fatal(err)
		}
		action, content, err := buildRulesContent(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if action != "created" {
			t.Fatalf("action = %q, want created", action)
		}
		if !strings.HasPrefix(content, preamble) {
			t.Fatal("append clobbered existing content")
		}
		if extractRulesBlock(content) != rulesContent {
			t.Fatal("appended content does not match canonical block")
		}
	})

	t.Run("marker without end marker is repaired", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "dex.md")
		// Truncated block: start marker present, end marker lost.
		truncated := rulesMarker + "\nhalf a block\n"
		if err := os.WriteFile(path, []byte(truncated), 0o644); err != nil {
			t.Fatal(err)
		}
		action, content, err := buildRulesContent(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if action != "updated" {
			t.Fatalf("action = %q, want updated", action)
		}
		if extractRulesBlock(content) != rulesContent {
			t.Fatal("repaired content does not match canonical block")
		}
	})
}
