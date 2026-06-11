package ignore

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// indexConfig is the parsed `index:` section of .dex/config.yml. Both lists
// carry gitignore-grammar patterns (see Matcher): Include is the opt-in
// allow-list of paths to index; Ignore composes on top of the
// .gitignore/.dexignore exclude set.
type indexConfig struct {
	Include []string
	Ignore  []string
}

// dexConfigFile is the subset of .dex/config.yml that the ignore package
// reads. Other sections (endpoints/models/tools/env, owned by cmd/dex) are
// ignored here — yaml.Unmarshal silently skips unknown keys.
type dexConfigFile struct {
	Index struct {
		Include []string `yaml:"include"`
		Ignore  []string `yaml:"ignore"`
	} `yaml:"index"`
}

// loadIndexConfig reads the `index:` section of .dex/config.yml under root.
// A missing file is not an error — a zero-value config is returned (which,
// under the opt-in model, means "index nothing").
//
//	index:
//	  include:
//	    - cmd/
//	    - internal/
//	    - "*.md"
//	  ignore:
//	    - testdata/
//	    - benchmark/results/
func loadIndexConfig(root string) (indexConfig, error) {
	var cfg indexConfig
	path := filepath.Join(root, ".dex", "config.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, err
	}
	var f dexConfigFile
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	cfg.Include = f.Index.Include
	cfg.Ignore = f.Index.Ignore
	return cfg, nil
}
