// Package workspace loads .dex/workspace.yml and provides multi-project
// configuration for workspace-scoped search.
package workspace

import (
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config is the parsed content of .dex/workspace.yml.
type Config struct {
	Projects []ProjectEntry `yaml:"projects"`
}

// ProjectEntry is one project in the workspace.
type ProjectEntry struct {
	// Path is the absolute or relative (to workspace root) path to the project.
	Path string `yaml:"path"`
	// Label is a short human-readable name for this project.
	// Defaults to the base directory name when omitted.
	Label string `yaml:"label,omitempty"`
}

// Load reads .dex/workspace.yml from root and returns the parsed config.
// Returns (nil, nil) when the file does not exist — callers treat that as
// "no workspace configured".
func Load(root string) (*Config, error) {
	p := filepath.Join(root, ".dex", "workspace.yml")
	data, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	// Resolve relative paths and fill in default labels.
	for i := range cfg.Projects {
		pe := &cfg.Projects[i]
		if !filepath.IsAbs(pe.Path) {
			pe.Path = filepath.Join(root, pe.Path)
		}
		if pe.Label == "" {
			pe.Label = filepath.Base(pe.Path)
		}
	}
	return &cfg, nil
}

