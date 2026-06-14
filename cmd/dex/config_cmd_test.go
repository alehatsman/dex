package main

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// parsedInclude mirrors the subset of .dex/config.yml the indexer reads.
type parsedInclude struct {
	Index struct {
		Include []string `yaml:"include"`
	} `yaml:"index"`
}

// TestBuildConfigYAMLActiveInclude proves the scaffolded config ships an active
// index.include defaulting to the project root, so `dex index .` indexes out of
// the box rather than 0 chunks (#546).
func TestBuildConfigYAMLActiveInclude(t *testing.T) {
	var got parsedInclude
	if err := yaml.Unmarshal([]byte(buildConfigYAML(false)), &got); err != nil {
		t.Fatalf("scaffolded config is not valid YAML: %v", err)
	}
	if len(got.Index.Include) != 1 || got.Index.Include[0] != "**" {
		t.Errorf("index.include = %v, want [**] (active by default)", got.Index.Include)
	}
}

// TestScaffoldConfigIfAbsent covers create-when-missing and leave-when-present.
func TestScaffoldConfigIfAbsent(t *testing.T) {
	dir := t.TempDir()

	path, created, err := scaffoldConfigIfAbsent(dir)
	if err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	if !created {
		t.Error("created = false on a fresh dir, want true")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config not written: %v", err)
	}
	if want := filepath.Join(dir, ".dex", "config.yml"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}

	// Second call must not overwrite an existing config.
	if err := os.WriteFile(path, []byte("index:\n  include:\n    - custom\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, created, err = scaffoldConfigIfAbsent(dir)
	if err != nil {
		t.Fatalf("scaffold (existing): %v", err)
	}
	if created {
		t.Error("created = true over an existing config, want false")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "index:\n  include:\n    - custom\n" {
		t.Error("scaffold overwrote an existing config")
	}
}
