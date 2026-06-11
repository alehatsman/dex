package eval

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// blastRadiusAnchorChars caps the query excerpt drawn from an anchor file.
// Long enough to identify the file semantically, short enough to behave like
// a focused "here's the code I'm editing" query.
const blastRadiusAnchorChars = 600

// blastRadiusAnchorLines caps how many meaningful lines the excerpt spans.
const blastRadiusAnchorLines = 18

// GenerateBlastRadius mines git history for co-change / blast-radius relevance.
// For each non-merge commit touching 2..opts.MaxFiles code files (all still on
// disk), every touched file becomes an anchor: the query is a code excerpt of
// that anchor's *current* content and the relevant set is the OTHER code files
// the same commit touched — "given I'm editing X, what changes with it?".
//
// Unlike the git-history golden set (commit subject → touched files, which are
// direct lexical/semantic hits), this rewards retrieving structurally-related
// files that are NOT direct matches — the graph lane's job (#248). The query
// excerpt comes from the file as it exists now so it matches the live index.
func GenerateBlastRadius(ctx context.Context, root string, opts GenOpts) (GoldenSet, error) {
	if opts.MaxCommits <= 0 {
		opts.MaxCommits = genDefaultMaxCommits
	}
	if opts.MaxFiles <= 0 {
		opts.MaxFiles = genDefaultMaxFiles
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
	seenAnchor := make(map[string]bool) // dedup (anchor, relevant-set) across commits
	for _, c := range commits {
		// Code files this commit touched that still exist on disk.
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
		// Need at least 2 co-changed files for a blast-radius example, and
		// cap commit size so a sprawling refactor doesn't inject noise.
		if len(files) < 2 || len(files) > opts.MaxFiles {
			continue
		}
		sort.Strings(files)

		for i, anchor := range files {
			// Relevant = the other co-changed files (anchor excluded).
			relevant := make([]string, 0, len(files)-1)
			for j, f := range files {
				if j != i {
					relevant = append(relevant, f)
				}
			}
			// Dedup identical (anchor → relevant-set) examples that recur
			// across commits (e.g. a file pair that changes together often).
			key := anchor + "→" + strings.Join(relevant, ",")
			if seenAnchor[key] {
				continue
			}
			excerpt := codeExcerpt(filepath.Join(root, anchor))
			if excerpt == "" {
				continue
			}
			seenAnchor[key] = true
			queries = append(queries, GoldenQuery{
				ID:            c.shortHash + ":" + filepath.Base(anchor),
				Query:         excerpt,
				RelevantFiles: relevant,
				Anchor:        anchor,
			})
		}
	}

	return GoldenSet{
		Repo:    filepath.Base(root),
		Head:    head,
		GenOpts: opts,
		Queries: queries,
	}, nil
}

// codeExcerpt reads up to the first blastRadiusAnchorLines meaningful lines of
// a source file (skipping blank lines and obvious comment lines) and returns
// them joined, capped at blastRadiusAnchorChars. Empty string if the file is
// unreadable or has no usable code. Deterministic for a given file content.
func codeExcerpt(absPath string) string {
	data, err := os.ReadFile(absPath)
	if err != nil {
		return ""
	}
	var picked []string
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || isCommentLine(line) {
			continue
		}
		picked = append(picked, line)
		if len(picked) >= blastRadiusAnchorLines {
			break
		}
	}
	out := strings.Join(picked, "\n")
	if len(out) > blastRadiusAnchorChars {
		out = out[:blastRadiusAnchorChars]
	}
	return out
}

// isCommentLine reports whether a trimmed line is an obvious single-line
// comment in the languages dex indexes (//, #, *, /*, --). Coarse on purpose:
// the goal is a code-dominated excerpt, not perfect lexing.
func isCommentLine(line string) bool {
	for _, p := range []string{"//", "#", "*", "/*", "--"} {
		if strings.HasPrefix(line, p) {
			return true
		}
	}
	return false
}
