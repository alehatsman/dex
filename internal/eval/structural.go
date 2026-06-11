package eval

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// structuralMinWords is the minimum word count a cleaned commit subject must
// have to be considered prose.
const structuralMinWords = 3

// structuralDefaultMaxFiles caps commits for structural generation. Higher than
// genDefaultMaxFiles (5) because larger cross-cutting commits are exactly the
// structurally-coupled changes the instrument is designed to measure.
const structuralDefaultMaxFiles = 10

// GenerateStructural mines git history for cross-package structural coupling.
//
// Query = cleaned commit subject (prose, no code, no imports).
// Relevant = all co-changed code files.
//
// Two filters ensure the set is sensitive to the graph-proximity lane:
//
//  1. Cross-package: files must span ≥2 distinct package directories.
//     Same-package changes are direct semantic matches of the commit subject
//     and do not test structural coupling.
//
//  2. Mixed lexicality: at least one relevant file must NOT be directly named
//     by the commit subject's tokens. Files that are not lexically named can
//     only be surfaced by structural graph traversal from the named files —
//     the graph lane's job. If the subject names ALL relevant files, BM25+dense
//     already finds them and graph proximity adds nothing measurable.
//
// Why import coupling is NOT filtered out (contrast with the previous design):
// the graph lane works by boosting neighbors of already-found files. Import
// edges between changed files ARE the graph structure the lane needs to
// traverse — removing those commits eliminates the very signal we want to
// measure and leaves fewer than 15 queries from dex's own history.
func GenerateStructural(ctx context.Context, root string, opts GenOpts) (GoldenSet, error) {
	if opts.MaxCommits <= 0 {
		opts.MaxCommits = genDefaultMaxCommits
	}
	if opts.MaxFiles <= 0 {
		opts.MaxFiles = structuralDefaultMaxFiles
	}

	head, err := gitOutput(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		return GoldenSet{}, fmt.Errorf("eval: read HEAD: %w", err)
	}
	commits, err := collectCommits(ctx, root, opts.MaxCommits)
	if err != nil {
		return GoldenSet{}, fmt.Errorf("eval: collect commits: %w", err)
	}

	var queries []GoldenQuery
	seen := make(map[string]bool)
	for _, c := range commits {
		q := cleanSubject(c.subject)
		if len(q) < genMinQueryLen || !isProseSubject(q) || seen[q] {
			continue
		}

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

		// Filter 1: files must span ≥2 distinct package directories.
		if distinctDirs(files) < 2 {
			continue
		}

		// Filter 2: at least one file must not be directly named by subject
		// tokens. If the subject names ALL files, BM25/dense will find them
		// all regardless — the set adds no graph-lane signal.
		if !hasStructuralTarget(q, files) {
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

// hasStructuralTarget returns true if at least one file in files is NOT
// directly named by the query text. "Named" means at least one of the file's
// path tokens (dir components, basename without extension) appears as a
// substring in the lowercased query, with a minimum token length of 4 to
// avoid false positives from short common words ("go", "mcp", "api").
func hasStructuralTarget(query string, files []string) bool {
	qLower := strings.ToLower(query)
	for _, f := range files {
		if !subjectNamesFile(qLower, f) {
			return true
		}
	}
	return false
}

// subjectNamesFile reports whether any path token derived from the file
// appears in the lowercased subject. Tokens shorter than 4 chars are skipped
// to reduce false positives from short package names.
func subjectNamesFile(subjectLower, path string) bool {
	for _, tok := range pathTokens(path) {
		if len(tok) < 4 {
			continue
		}
		if strings.Contains(subjectLower, tok) {
			return true
		}
	}
	return false
}

// pathTokens returns all non-empty directory components and the basename
// (without extension) of a repo-relative file path, lowercased.
func pathTokens(path string) []string {
	dir, base := filepath.Split(filepath.ToSlash(path))
	ext := filepath.Ext(base)
	base = strings.TrimSuffix(base, ext)

	var tokens []string
	for _, part := range strings.Split(strings.Trim(dir, "/"), "/") {
		if part != "" {
			tokens = append(tokens, strings.ToLower(part))
		}
	}
	if base != "" {
		tokens = append(tokens, strings.ToLower(base))
	}
	return tokens
}

// isProseSubject reports whether a cleaned commit subject looks like
// natural-language prose: ≥3 words and no code-like token patterns.
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
