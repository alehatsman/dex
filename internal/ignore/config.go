package ignore

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/alehatsman/dex/internal/gitworktree"
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
	cfg, found, err := readIndexConfig(root)
	if err != nil || found {
		return cfg, err
	}
	// #108: a linked git worktree has no .dex/config.yml of its own, so indexing
	// is opt-in against an empty include list — nothing gets indexed. Inherit the
	// main working tree's config so a worktree resolves exactly like its parent.
	if main, ok := gitworktree.MainWorktree(root); ok {
		return loadIndexConfig(main)
	}
	return cfg, nil
}

// readIndexConfig reads and parses root's .dex/config.yml. found is false when
// the file does not exist (an empty, index-nothing config — the caller may then
// fall back to an inherited config).
func readIndexConfig(root string) (cfg indexConfig, found bool, err error) {
	path := filepath.Join(root, ".dex", "config.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, false, nil
		}
		return cfg, false, err
	}
	var f dexConfigFile
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return cfg, false, fmt.Errorf("%s: %w", path, err)
	}
	cfg.Include = f.Index.Include
	cfg.Ignore = f.Index.Ignore
	return cfg, true, nil
}
