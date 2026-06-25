package compress

import (
	"fmt"
	"sort"
	"strings"
)

// ExtractGoImportBlock finds the first parenthesized import block in a Go
// source file. Returns the text before and after the block, the sorted import
// paths extracted from it, the number of lines the block occupied (including
// the "import (" and ")" delimiters), and whether a block was found.
//
// Single-import forms ("import \"fmt\"") and files with no parenthesized block
// return ok=false. Only the first parenthesized block is extracted; well-formed
// Go files have at most one.
func ExtractGoImportBlock(src string) (before, after string, paths []string, blockLines int, ok bool) {
	lines := strings.Split(src, "\n")
	start, end := -1, -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "import (" {
			start = i
		} else if start >= 0 && strings.TrimSpace(line) == ")" {
			end = i
			break
		}
	}
	if start < 0 || end < 0 {
		return "", "", nil, 0, false
	}

	before = strings.Join(lines[:start], "\n")
	after = strings.Join(lines[end+1:], "\n")
	blockLines = end - start + 1

	seen := map[string]bool{}
	for _, line := range lines[start+1 : end] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") {
			continue
		}
		// The import path is always the last whitespace-delimited token (handles
		// aliased imports like `foo "github.com/foo/bar"`).
		parts := strings.Fields(trimmed)
		path := strings.Trim(parts[len(parts)-1], `"`)
		if path != "" && !seen[path] {
			seen[path] = true
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return before, after, paths, blockLines, true
}

// DeduplicateGoImports deduplicates Go import blocks across a batch of file
// contents. When at least 2 files have a parenthesized import block the union
// of all import paths is emitted as a shared header comment and each file's
// block is replaced with a single back-reference line.
//
// Returns:
//   - sharedHeader: the shared import block text to prepend to the batch; empty
//     when fewer than 2 files had import blocks (dedup was skipped).
//   - deduped: per-file content with import blocks replaced; identical to the
//     input slice when dedup was skipped.
//   - savedLines: total import lines removed across all files (the shared header
//     is counted separately and not subtracted here).
func DeduplicateGoImports(files []string) (sharedHeader string, deduped []string, savedLines int) {
	type result struct {
		before, after string
		paths         []string
		blockLines    int
		ok            bool
	}

	results := make([]result, len(files))
	eligible := 0
	for i, f := range files {
		before, after, paths, blockLines, ok := ExtractGoImportBlock(f)
		if ok {
			eligible++
			results[i] = result{before: before, after: after, paths: paths, blockLines: blockLines, ok: true}
		}
	}

	if eligible < 2 {
		return "", files, 0
	}

	// Union of all import paths, deduplicated and sorted.
	seen := map[string]bool{}
	var union []string
	for _, r := range results {
		if !r.ok {
			continue
		}
		for _, p := range r.paths {
			if !seen[p] {
				seen[p] = true
				union = append(union, p)
			}
		}
	}
	sort.Strings(union)

	// Emit the shared import block with a leading count comment.
	var hb strings.Builder
	fmt.Fprintf(&hb, "// Shared imports (%d files):\nimport (\n", eligible)
	for _, p := range union {
		fmt.Fprintf(&hb, "\t%q\n", p)
	}
	hb.WriteString(")\n")
	sharedHeader = hb.String()

	// Rebuild per-file content with the import block replaced.
	deduped = make([]string, len(files))
	for i, f := range files {
		r := results[i]
		if !r.ok {
			deduped[i] = f
			continue
		}
		// Splice: before + replacement comment + after.
		// before/after already have the correct surrounding newlines from the
		// split in ExtractGoImportBlock (the blank line after the import block
		// survives in after).
		deduped[i] = r.before + "\n// imports: see shared header above\n" + r.after
		// blockLines lines replaced by 1 → (blockLines - 1) lines saved.
		savedLines += r.blockLines - 1
	}

	return sharedHeader, deduped, savedLines
}
