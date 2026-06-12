package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, root, body string) {
	t.Helper()
	dir := filepath.Join(root, ".dex")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestParseConfigFile(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, `
endpoints:
  embed: http://embed:11434
  chat: http://chat:11434
models:
  embed: mxbai-embed-large
tools:
  embed_batch: 8
  disable_rerank: true
env:
  DEX_EMBED_CONCURRENCY: 16
  DEX_SERVE_TOKEN: should-be-ignored
`)
	got, err := parseConfigFile(root)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"DEX_EMBED_URL":         "http://embed:11434",
		"DEX_CHAT_URL":          "http://chat:11434",
		"DEX_EMBED_MODEL":       "mxbai-embed-large",
		"DEX_EMBED_BATCH":       "8",
		"DEX_DISABLE_RERANK":    "1", // bool true -> "1"
		"DEX_EMBED_CONCURRENCY": "16",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if _, ok := got["DEX_SERVE_TOKEN"]; ok {
		t.Error("DEX_SERVE_TOKEN must never be read from config.yml")
	}
	if len(got) != len(want) {
		t.Errorf("got %d keys, want %d: %v", len(got), len(want), got)
	}
}

func TestParseConfigFileMissing(t *testing.T) {
	got, err := parseConfigFile(t.TempDir())
	if err != nil {
		t.Fatalf("missing file must not error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("missing file should yield empty map, got %v", got)
	}
}

func TestApplyProjectConfigEnvWins(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, `
endpoints:
  embed: http://from-file:11434
models:
  chat: from-file-model
`)
	// DEX_EMBED_URL is already set in the environment — file must NOT override.
	t.Setenv("DEX_EMBED_URL", "http://from-env:11434")
	// DEX_CHAT_MODEL is unset — file should fill it.
	os.Unsetenv("DEX_CHAT_MODEL")
	t.Cleanup(func() { os.Unsetenv("DEX_CHAT_MODEL"); delete(fileSourcedKeys, "DEX_CHAT_MODEL") })

	if err := applyProjectConfig(root); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("DEX_EMBED_URL"); got != "http://from-env:11434" {
		t.Errorf("env var was overridden by file: DEX_EMBED_URL = %q", got)
	}
	if fileSourcedKeys["DEX_EMBED_URL"] {
		t.Error("DEX_EMBED_URL was env-set; must not be marked file-sourced")
	}
	if got := os.Getenv("DEX_CHAT_MODEL"); got != "from-file-model" {
		t.Errorf("unset var not filled from file: DEX_CHAT_MODEL = %q", got)
	}
	if !fileSourcedKeys["DEX_CHAT_MODEL"] {
		t.Error("DEX_CHAT_MODEL should be marked file-sourced")
	}
}
