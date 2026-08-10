package ignore

import (
	"io/fs"
	"path/filepath"
	"strings"
)

// nestedWalkSkip are directories collectNestedGitignore never descends into —
// they never hold project-relevant nested .gitignore rules and would only slow
// the collection walk on large trees.
var nestedWalkSkip = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, ".dex": true,
}

// collectNestedGitignore walks root once and returns the re-anchored patterns of
// every nested (non-root) .gitignore, in walk order (shallower directories
// first, so a deeper file's rules land later and win). The root .gitignore is
// skipped — New reads it directly. Opt-in (#74).
func collectNestedGitignore(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if path == root {
				return walkErr
			}
			return nil // skip unreadable subtrees
		}
		if d.IsDir() {
			if path != root && nestedWalkSkip[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != ".gitignore" {
			return nil
		}
		dir := filepath.ToSlash(relDir(root, path))
		if dir == "" {
			return nil // the root .gitignore, already read by New
		}
		lines, err := readLines(path)
		if err != nil {
			return err
		}
		for _, ln := range lines {
			if p := reanchorPattern(dir, ln); p != "" {
				out = append(out, p)
			}
		}
		return nil
	})
	return out, err
}

// relDir returns the directory of a .gitignore file relative to root, in the
// host separator; "" for the root file.
func relDir(root, gitignorePath string) string {
	rel, err := filepath.Rel(root, filepath.Dir(gitignorePath))
	if err != nil || rel == "." {
		return ""
	}
	return rel
}

// reanchorPattern rewrites one line of a nested .gitignore (living in dir,
// slash-relative to root) into a root-relative pattern, following git anchoring
// rules:
//
//   - a blank or comment line yields "";
//   - a pattern containing a non-trailing '/' is anchored to dir:
//     "/x" and "a/b" in dir "p/q" become "p/q/x" and "p/q/a/b";
//   - an unanchored pattern matches at any depth below dir:
//     "x" in "p/q" becomes "p/q/**/x";
//   - a trailing '/' (dir-only marker) and a leading '!' (negation) are
//     preserved.
//
// This approximates a few negation edge-cases, acceptable for an opt-in
// convenience (#74).
func reanchorPattern(dir, line string) string {
	p := strings.TrimSpace(strings.TrimSuffix(line, "\r"))
	if p == "" || strings.HasPrefix(p, "#") {
		return ""
	}
	neg := ""
	if strings.HasPrefix(p, "!") {
		neg, p = "!", strings.TrimPrefix(p, "!")
	}
	if p == "" {
		return ""
	}

	trailingSlash := strings.HasSuffix(p, "/")
	core := strings.TrimSuffix(p, "/")
	anchored := strings.Contains(core, "/") // a '/' at start or middle anchors to dir
	core = strings.TrimPrefix(core, "/")

	var b strings.Builder
	b.WriteString(neg)
	b.WriteString(dir)
	b.WriteByte('/')
	if !anchored {
		b.WriteString("**/")
	}
	b.WriteString(core)
	if trailingSlash {
		b.WriteByte('/')
	}
	return b.String()
}
