package mcp

import (
	"os"
	"path/filepath"

	"github.com/alehatsman/dex/internal/ignore"
)

// walkProjectFiles enumerates the ignore-filtered working-tree files under
// searchRel (project-relative; "" = whole project), returning absolute paths.
//
// This is the authority for "which files exist in the project." grep and
// file_tree build on it rather than on the chunk index, so a file that is on
// disk but carries no chunks — a generated file kept lean, an over-density-cap
// file, or one indexed only later — stays visible instead of vanishing from
// grep/ls (#132). The chunk index remains the authority for the semantic layers
// (search/trace) and, for file_tree, only for per-file chunk counts.
//
// The exclude set is defaults + .gitignore/.dexignore + config ignore (the same
// MatchExclude the grep fallback walk has always used); the opt-in include
// allow-list is deliberately skipped so enumeration still works in a project
// with no .dex/config.yml. A directory explicitly targeted as searchRel is never
// self-excluded. Returns the walk error only when the search root itself is
// unwalkable (e.g. a bad path); inaccessible subdirectories are skipped.
func walkProjectFiles(root, searchRel string) ([]string, error) {
	searchRoot := root
	if searchRel != "" {
		searchRoot = filepath.Join(root, searchRel)
	}
	matcher, _ := ignore.New(root)
	var out []string
	err := filepath.Walk(searchRoot, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			if path == searchRoot {
				return walkErr // propagate root-level errors (e.g. path doesn't exist)
			}
			return nil // skip inaccessible subdirectories
		}
		rel, relErr := filepath.Rel(root, path)
		if info.IsDir() {
			switch info.Name() {
			case ".git", "vendor", "node_modules", ".dex":
				return filepath.SkipDir
			}
			// Never self-exclude the explicitly requested search root: if the
			// caller scoped into an ignored dir, honor it.
			if matcher != nil && relErr == nil && path != searchRoot && matcher.MatchExclude(rel, true) {
				return filepath.SkipDir
			}
			return nil
		}
		if matcher != nil && relErr == nil && matcher.MatchExclude(rel, false) {
			return nil
		}
		out = append(out, path)
		return nil
	})
	return out, err
}
