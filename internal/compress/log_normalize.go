package compress

import (
	"fmt"
	"regexp"
	"strings"
)

// ── normalizers ───────────────────────────────────────────────────────────────

var (
	reTS   = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})?`)
	reHex  = regexp.MustCompile(`\b[0-9a-f]{32,}\b`)
	reUUID = regexp.MustCompile(`\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	reSep  = regexp.MustCompile(`[-_]{3,}`)
)

func NormalizeTimestamps(s string) string { return reTS.ReplaceAllString(s, "[TS]") }
func NormalizeHashes(s string) string     { return reHex.ReplaceAllString(s, "[HASH]") }
func NormalizeUUIDs(s string) string      { return reUUID.ReplaceAllString(s, "[UUID]") }

// NormalizeLineForDedup normalizes a log line for dedup comparison:
// timestamps → [TS], UUIDs → [UUID], hex hashes → [HASH].
//
// The log-level token is deliberately NOT stripped: severity is signal, not
// per-line noise. Stripping it made "INFO connection failed" and "ERROR
// connection failed" normalize identically, so dedup dropped the ERROR line
// (or mislabeled it as INFO in a run count). TS/UUID/HASH stripping already
// collapses genuine repeated spam, which is almost always at a single level.
func NormalizeLineForDedup(s string) string {
	s = NormalizeTimestamps(s)
	s = NormalizeUUIDs(s)
	s = NormalizeHashes(s)
	s = strings.TrimSpace(s)
	return s
}

// IsBoilerplateLine returns true for common log boilerplate that carries no
// unique information per line.
func IsBoilerplateLine(s string) bool {
	t := strings.ToLower(strings.TrimSpace(s))
	for _, b := range []string{
		"starting", "started", "stopping", "stopped", "shutting down",
		"initializing", "initialized", "listening on", "ready",
	} {
		if strings.Contains(t, b) {
			return true
		}
	}
	return false
}

// VerbatimCompact deduplicates consecutive runs of identical normalized lines,
// annotating with [Nx] run counts.
func VerbatimCompact(lines []string) []string {
	if len(lines) == 0 {
		return nil
	}
	type run struct {
		line  string
		norm  string
		count int
	}
	var runs []run
	for _, l := range lines {
		norm := NormalizeLineForDedup(l)
		if len(runs) > 0 && runs[len(runs)-1].norm == norm {
			runs[len(runs)-1].count++
		} else {
			runs = append(runs, run{line: l, norm: norm, count: 1})
		}
	}
	var out []string
	for _, r := range runs {
		if r.count == 1 {
			out = append(out, r.line)
		} else {
			out = append(out, fmt.Sprintf("[%dx] %s", r.count, r.line))
		}
	}
	return out
}

// CompressLogDedup compresses log output by normalizing and deduplicating lines.
// Returns nil if no meaningful compression was achieved.
func CompressLogDedup(lines []string) []string {
	if len(lines) < 10 {
		return nil
	}
	compacted := VerbatimCompact(lines)
	if len(compacted) >= len(lines) {
		return nil
	}
	return compacted
}

// ── log block compressor ──────────────────────────────────────────────────────

var (
	reBlockHeader  = &lazyRe{pattern: `(?i)(error|warn|fatal|panic|exception|traceback|crash)`}
	reGitCommitHdr = &lazyRe{pattern: `^commit [0-9a-f]{7,40}`}
	reGitDiffHdrB  = &lazyRe{pattern: `^(diff --git|--- a/|\+\+\+ b/|index )`}
)

// IsBlockBoundary returns true if the line looks like a structural log boundary.
func IsBlockBoundary(l string) bool {
	t := strings.TrimSpace(l)
	if t == "" || reSep.MatchString(t) {
		return true
	}
	if reGitCommitHdr.MatchString(t) || reGitDiffHdrB.MatchString(t) {
		return true
	}
	return false
}

// IsErrorLogLine returns true if the line looks like an error/warning log entry.
func IsErrorLogLine(l string) bool {
	return reBlockHeader.MatchString(l)
}

// DedupBlockLines deduplicates similar lines within a block.
func DedupBlockLines(lines []string) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, l := range lines {
		norm := NormalizeLineForDedup(l)
		if _, ok := seen[norm]; ok {
			continue
		}
		seen[norm] = struct{}{}
		out = append(out, l)
	}
	return out
}

// CompressLogBlock detects and compresses repetitive log block output.
// Returns nil if the input doesn't look like log block output.
func CompressLogBlock(lines []string) []string {
	if len(lines) < 20 {
		return nil
	}
	errorCount := 0
	for _, l := range lines {
		if IsErrorLogLine(l) {
			errorCount++
		}
	}
	// Only operate on log-heavy output (at least 20% error/warn lines).
	if float64(errorCount)/float64(len(lines)) < 0.20 {
		return nil
	}
	// Split into blocks and deduplicate each.
	var blocks [][]string
	var current []string
	for _, l := range lines {
		if IsBlockBoundary(l) && len(current) > 0 {
			blocks = append(blocks, current)
			current = nil
		}
		current = append(current, l)
	}
	if len(current) > 0 {
		blocks = append(blocks, current)
	}
	var out []string
	for _, block := range blocks {
		deduped := DedupBlockLines(block)
		out = append(out, deduped...)
	}
	if len(out) >= len(lines) {
		return nil
	}
	return out
}
