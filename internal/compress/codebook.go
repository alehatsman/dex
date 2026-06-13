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
	// entries are ordered most-frequent-first (then lexicographically) for
	// deterministic §N assignment. Apply matches whole trimmed lines exactly,
	// so entry order does not affect correctness.
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
			// Key on the fully-trimmed line so dedup is indent-insensitive and
			// the stored §N value is indent-free — matching Apply, which re-adds
			// each line's own indent ahead of the ref.
			line = strings.TrimSpace(line)
			if len(line) < 8 {
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
		// Match on the fully-trimmed line — entries are keyed indent-free.
		trimmed := strings.TrimSpace(line)
		for _, e := range cb.entries {
			if trimmed == e.line {
				// Re-add this line's own leading whitespace (indent) ahead of
				// the ref so expansion reproduces the original exactly.
				indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
				lines[i] = indent + e.ref
				break
			}
		}
	}
	return strings.Join(lines, "\n")
}
