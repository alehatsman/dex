package compress

import (
	"fmt"
	"sort"
	"strings"
)

// Codebook replaces repeated lines across multiple files with short §N refs,
// reducing token cost when the same boilerplate (imports, error patterns,
// license lines) appears in 3+ files.
type Codebook struct {
	// entries are sorted longest-first to avoid partial-match conflicts.
	entries []codebookEntry
}

type codebookEntry struct {
	line string
	ref  string // §0, §1, …
}

// BuildCodebook scans the combined text of multiple files and returns a
// Codebook for any line that appears in 3+ distinct files.
// Returns an empty Codebook (no-op Apply) when there are fewer than 3 files
// or total line count exceeds 50 000 (memory guard).
func BuildCodebook(files []string) Codebook {
	if len(files) < 3 {
		return Codebook{}
	}

	// Count total lines as a cheap pre-guard.
	total := 0
	for _, f := range files {
		total += strings.Count(f, "\n") + 1
	}
	if total > 50_000 {
		return Codebook{}
	}

	// df = how many files contain each line (document frequency).
	df := make(map[string]int)
	for _, f := range files {
		seen := make(map[string]bool)
		for _, line := range strings.Split(f, "\n") {
			line = strings.TrimRight(line, " \t")
			if len(strings.TrimSpace(line)) < 8 {
				// Skip very short lines — not worth encoding.
				continue
			}
			if !seen[line] {
				df[line]++
				seen[line] = true
			}
		}
	}

	// Collect lines that appear in ≥3 files.
	type candidate struct {
		line string
		freq int
	}
	var candidates []candidate
	for line, freq := range df {
		if freq >= 3 {
			candidates = append(candidates, candidate{line, freq})
		}
	}
	if len(candidates) == 0 {
		return Codebook{}
	}

	// Sort by frequency desc, then lexicographically for determinism.
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].freq != candidates[j].freq {
			return candidates[i].freq > candidates[j].freq
		}
		return candidates[i].line < candidates[j].line
	})

	// Limit to top 50 entries.
	if len(candidates) > 50 {
		candidates = candidates[:50]
	}

	// Sort longest-first to prevent partial-match conflicts during Apply.
	sort.Slice(candidates, func(i, j int) bool {
		return len(candidates[i].line) > len(candidates[j].line)
	})

	entries := make([]codebookEntry, len(candidates))
	for i, c := range candidates {
		entries[i] = codebookEntry{line: c.line, ref: fmt.Sprintf("§%d", i)}
	}
	return Codebook{entries: entries}
}

// Empty returns true when the codebook has no entries (Apply is a no-op).
func (cb Codebook) Empty() bool { return len(cb.entries) == 0 }

// Legend returns the codebook header to prepend to the combined output.
// Format: "§0=<line>  §1=<line>  …\n"
func (cb Codebook) Legend() string {
	if cb.Empty() {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("§MAP:")
	for _, e := range cb.entries {
		sb.WriteString("\n  ")
		sb.WriteString(e.ref)
		sb.WriteByte('=')
		sb.WriteString(e.line)
	}
	sb.WriteByte('\n')
	return sb.String()
}

// Apply replaces each encoded line in text with its §N reference.
// Lines that are not in the codebook are left unchanged.
func (cb Codebook) Apply(text string) string {
	if cb.Empty() {
		return text
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		trimmed := strings.TrimRight(line, " \t")
		for _, e := range cb.entries {
			if trimmed == e.line {
				// Preserve leading whitespace (indent).
				indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
				lines[i] = indent + e.ref
				break
			}
		}
	}
	return strings.Join(lines, "\n")
}
