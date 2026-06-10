package eval

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// structuralMinWords is the minimum word count (by spaces) a cleaned commit
// subject must have to be considered prose. Single-word or two-word subjects
// are typically type names, file names, or version bumps — not useful queries.
const structuralMinWords = 3

// GenerateStructural mines git history for structural-coupling golden queries:
// prose commit subjects against co-changed files that have no direct import
// relationship between them.
//
// This is the instrument for tuning the graph-proximity lane (#279): the
// graph lane's job is to surface callers/callees/peers that are NOT lexical
// matches of the query — structural coupling invisible to BM25+dense.
//
// Selection criteria:
//   - Commit subject is prose (≥3 words, no code-like tokens after cleanup)
//   - Changed code files span ≥2 distinct packages (directories)
//   - No changed file imports any other changed file (cross-import check)
//
// The cross-import filter is the key: when fileA and fileB import each other,
// BM25 can find both from the commit subject (which tends to mention the
// shared types). Removing those pairs ensures only graph-mediated coupling
// remains in the set.
func GenerateStructural(ctx context.Context, root string, opts GenOpts) (GoldenSet, error) {
	if opts.MaxCommits <= 0 {
		opts.MaxCommits = genDefaultMaxCommits
	}
	if opts.MaxFiles <= 0 {
		opts.MaxFiles = genDefaultMaxFiles
	}

	head, err := gitOutput(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		return GoldenSet{}, err
	}
	commits, err := collectCommits(ctx, root, opts.MaxCommits)
	if err != nil {
		return GoldenSet{}, err
	}

	var queries []GoldenQuery
	seen := make(map[string]bool) // dedup identical query texts
	for _, c := range commits {
		q := cleanSubject(c.subject)
		if len(q) < genMinQueryLen || !isProseSubject(q) || seen[q] {
			continue
		}

		// Collect code files that still exist on disk.
		var files []string
		for _, f := range c.files {
			if !codeExts[strings.ToLower(filepath.Ext(f))] {
				continue
			}
			if _, statErr := os.Stat(filepath.Join(root, f)); statErr != nil {
				continue
			}
			files = append(files, f)
		}
		if len(files) < 2 || len(files) > opts.MaxFiles {
			continue
		}

		// Require files to span at least two distinct packages (directories).
		if distinctDirs(files) < 2 {
			continue
		}

		// Reject commits where any changed file imports any other: if imports
		// cross between the changed files, BM25/dense can already surface them
		// from the commit subject (which mentions the shared abstraction).
		if anyImportsCross(root, files) {
			continue
		}

		sort.Strings(files)
		seen[q] = true
		queries = append(queries, GoldenQuery{
			ID:            c.shortHash,
			Query:         q,
			RelevantFiles: files,
		})
	}

	return GoldenSet{
		Repo:    filepath.Base(root),
		Head:    head,
		GenOpts: opts,
		Queries: queries,
	}, nil
}

// isProseSubject reports whether a cleaned commit subject looks like
// natural-language prose rather than a code identifier or file path.
// We require ≥3 words and exclude strings that contain code-like token
// patterns ({}, (), []) that survive the conventional-commit prefix strip.
func isProseSubject(s string) bool {
	if strings.Count(s, " ") < structuralMinWords-1 {
		return false
	}
	for _, tok := range []string{"{", "}", "()", "[]"} {
		if strings.Contains(s, tok) {
			return false
		}
	}
	return true
}

// distinctDirs counts the number of unique parent directories among files.
func distinctDirs(files []string) int {
	dirs := make(map[string]bool, len(files))
	for _, f := range files {
		dirs[filepath.ToSlash(filepath.Dir(f))] = true
	}
	return len(dirs)
}

// anyImportsCross reports whether any file in the set references another
// file's package directory in an import-like line. This is a proxy for
// "fileA imports fileB's package" that works across Go, TS/JS, Python,
// Rust, and Java — all embed the package/module directory path in their
// import statement.
//
// We scan only lines that look like import lines: lines containing a quote
// character (Go, TS, JS, Python, Java) or starting with "use " (Rust) or
// "import " / "require". This avoids false positives from comments and docs
// that happen to mention another package's name.
func anyImportsCross(root string, files []string) bool {
	type entry struct {
		dir     string // filepath.ToSlash(filepath.Dir(file))
		content string
	}
	entries := make([]entry, 0, len(files))
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			continue
		}
		entries = append(entries, entry{
			dir:     filepath.ToSlash(filepath.Dir(f)),
			content: string(data),
		})
	}
	for i, a := range entries {
		for j, b := range entries {
			if i == j || a.dir == b.dir {
				continue
			}
			if importRefPresent(a.content, b.dir) {
				return true
			}
		}
	}
	return false
}

// importRefPresent reports whether content contains an import-like line that
// mentions dirPath. A line is import-like if it contains a quote character or
// starts with a recognized import keyword.
func importRefPresent(content, dirPath string) bool {
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if !strings.Contains(line, dirPath) {
			continue
		}
		if strings.ContainsAny(line, `"'`) ||
			strings.HasPrefix(line, "import ") ||
			strings.HasPrefix(line, "use ") ||
			strings.HasPrefix(line, "require") ||
			strings.HasPrefix(line, "from ") {
			return true
		}
	}
	return false
}
