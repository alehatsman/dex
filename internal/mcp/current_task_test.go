package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteCurrentTask_WritesFile(t *testing.T) {
	dir := t.TempDir()
	// Override UserCacheDir by writing into a known temp path directly.
	// writeCurrentTask uses os.UserCacheDir() so we patch via env.
	t.Setenv("XDG_CACHE_HOME", dir)

	writeCurrentTask("implement the foo feature")

	data, err := os.ReadFile(filepath.Join(dir, "dex", "current_task"))
	if err != nil {
		t.Fatalf("current_task not written: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != "implement the foo feature" {
		t.Errorf("current_task = %q, want %q", got, "implement the foo feature")
	}
}

func TestWriteCurrentTask_Overwrites(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", dir)

	writeCurrentTask("task one")
	writeCurrentTask("task two")

	data, _ := os.ReadFile(filepath.Join(dir, "dex", "current_task"))
	if got := strings.TrimSpace(string(data)); got != "task two" {
		t.Errorf("current_task = %q, want %q", got, "task two")
	}
}
