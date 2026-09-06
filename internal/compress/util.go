package compress

import (
	"fmt"
	"strings"

	"github.com/alehatsman/dex/internal/tokens"
)

// Package compress provides text-compression utilities for shell-output reduction.

// EstimateTokens returns a real BPE token count (default o200k_base) so the
// over-compression guard's ratio test triggers on the tokens the model
// actually sees rather than a whitespace-word approximation.
func EstimateTokens(s string) int { return tokens.Count(s) }

// safetyNeedles are patterns that must survive truncation.
var safetyNeedleStrs = []string{
	"CRITICAL", "FATAL", "panic", "FAILED", "unhealthy", "Exited", "OOMKilled",
	"DETACHED HEAD", "detached", "vulnerability", "CVE-", "denied", "unauthorized",
	"forbidden", "segfault", "Segmentation fault", "SIGSEGV", "SIGKILL", "killed",
	"out of memory", "stack overflow", "permission denied", "certificate", "expired",
	"corrupt", "test result:", "panicked", "assertion", "traceback", "tests run",
	"error", "warning", "failed", "passed", "passing",
}

// ExtractSafetyLines scans lines for safety needles and returns up to max
// matching lines (preserving order). Case-insensitive match.
func ExtractSafetyLines(lines []string, max int) []string {
	var out []string
	for _, l := range lines {
		if len(out) >= max {
			break
		}
		ll := strings.ToLower(l)
		for _, needle := range safetyNeedleStrs {
			if strings.Contains(ll, strings.ToLower(needle)) {
				out = append(out, l)
				break
			}
		}
	}
	return out
}

// ── generic ──────────────────────────────────────────────────────────────────

var (
	reProgressLine = &lazyRe{pattern: `(\d+%|[\d.]+/[\d.]+\s*(MB|KB|GB)|ETA|elapsed|\[=+>?\s*\])`}
	reTimestamp    = &lazyRe{pattern: `^\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}`}
)

func CompressGeneric(lines []string) []string {
	out := make([]string, 0, len(lines))
	prev := ""
	for _, l := range lines {
		// Strip leading timestamp for duplicate comparison so that repeated
		// log lines with different timestamps are still collapsed.
		key := l
		if loc := reTimestamp.FindStringIndex(l); loc != nil {
			key = strings.TrimSpace(l[loc[1]:])
		}
		if key == prev && key != "" {
			continue
		}
		// Drop pure progress lines (spinners, percentages, progress bars).
		if reProgressLine.MatchString(l) && len(l) < 120 {
			continue
		}
		out = append(out, l)
		prev = key
	}
	return out
}

// CollapseBlankLines collapses runs of 2+ blank lines into a single blank line.
func CollapseBlankLines(lines []string) []string {
	out := make([]string, 0, len(lines))
	blanks := 0
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			blanks++
			if blanks == 1 {
				out = append(out, l)
			}
		} else {
			blanks = 0
			out = append(out, l)
		}
	}
	return out
}

// CompactLines returns at most max non-blank lines from lines, with an ellipsis
// suffix if lines were omitted.
func CompactLines(lines []string, max int) []string {
	if len(lines) <= max {
		return lines
	}
	out := make([]string, 0, max+1)
	kept := 0
	for _, l := range lines {
		if kept >= max {
			break
		}
		out = append(out, l)
		kept++
	}
	omitted := len(lines) - kept
	out = append(out, fmt.Sprintf("[%d lines omitted]", omitted))
	return out
}

// ── grep ─────────────────────────────────────────────────────────────────────

var reGrepLine = &lazyRe{pattern: `^([^:]+):(\d+):(.*)$`}

func CompressGrep(lines []string) []string {
	type fileMatch struct {
		file  string
		count int
		lines []string
	}
	fileMap := make(map[string]*fileMatch)
	var fileOrder []string
	total := 0
	for _, l := range lines {
		m := reGrepLine.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		file := m[1]
		total++
		if _, ok := fileMap[file]; !ok {
			fileMap[file] = &fileMatch{file: file}
			fileOrder = append(fileOrder, file)
		}
		fm := fileMap[file]
		fm.count++
		if len(fm.lines) < 3 {
			fm.lines = append(fm.lines, strings.TrimSpace(m[3]))
		}
	}
	if total == 0 {
		return lines
	}
	header := fmt.Sprintf("%d matches in %dF:", total, len(fileOrder))
	out := []string{header}
	for _, file := range fileOrder {
		fm := fileMap[file]
		out = append(out, fmt.Sprintf("%s (%d):", file, fm.count))
		for _, match := range fm.lines {
			preview := match
			if len(preview) > 80 {
				preview = preview[:80] + "…"
			}
			out = append(out, "  "+preview)
		}
		if fm.count > 3 {
			out = append(out, fmt.Sprintf("  … +%d more", fm.count-3))
		}
	}
	return out
}

// ── find ─────────────────────────────────────────────────────────────────────

var reFindSkip = &lazyRe{pattern: `^find: |Permission denied|No such file`}

func CompressFind(lines []string) []string {
	var paths []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" || reFindSkip.MatchString(t) {
			continue
		}
		paths = append(paths, t)
	}
	if len(paths) == 0 {
		return lines
	}
	// Group by directory prefix.
	dirMap := make(map[string][]string)
	var dirOrder []string
	for _, p := range paths {
		dir := p
		if idx := strings.LastIndex(p, "/"); idx > 0 {
			dir = p[:idx]
		}
		if _, ok := dirMap[dir]; !ok {
			dirOrder = append(dirOrder, dir)
		}
		dirMap[dir] = append(dirMap[dir], p)
	}
	files := len(paths)
	dirs := len(dirOrder)
	header := fmt.Sprintf("%dF in %dD:", files, dirs)
	out := []string{header}
	for _, dir := range dirOrder {
		dirPaths := dirMap[dir]
		out = append(out, dir+"/")
		shown := dirPaths
		if len(shown) > 5 {
			shown = shown[:5]
		}
		for _, p := range shown {
			base := p
			if idx := strings.LastIndex(p, "/"); idx >= 0 {
				base = p[idx+1:]
			}
			out = append(out, "  "+base)
		}
		if len(dirPaths) > 5 {
			out = append(out, fmt.Sprintf("  … +%d more", len(dirPaths)-5))
		}
	}
	return out
}

// ── helpers ───────────────────────────────────────────────────────────────────

func ParseInt(s string) int {
	s = strings.TrimSpace(s)
	n := 0
	for _, c := range s {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		} else {
			break
		}
	}
	return n
}

func ParseFloat(s string) float64 {
	var n float64
	_, _ = fmt.Sscanf(strings.TrimSpace(s), "%f", &n) // best-effort: 0 on parse failure
	return n
}

func ParseUint64(s string) uint64 {
	var n uint64
	for _, c := range strings.TrimSpace(s) {
		if c >= '0' && c <= '9' {
			n = n*10 + uint64(c-'0')
		} else {
			break
		}
	}
	return n
}

func ParseSizeField(s string) uint64 {
	s = strings.TrimSpace(s)
	var v uint64
	if _, err := fmt.Sscanf(s, "%d", &v); err == nil {
		return v
	}
	if len(s) == 0 {
		return 0
	}
	last := s[len(s)-1]
	prefix := s[:len(s)-1]
	var base float64
	_, _ = fmt.Sscanf(prefix, "%f", &base) // best-effort: 0 on parse failure
	switch last {
	case 'K', 'k':
		return uint64(base * 1024)
	case 'M', 'm':
		return uint64(base * 1024 * 1024)
	case 'G', 'g':
		return uint64(base * 1024 * 1024 * 1024)
	}
	_, _ = fmt.Sscanf(s, "%d", &v) // best-effort: 0 on parse failure
	return v
}

func FormatDuSize(bytes uint64) string {
	switch {
	case bytes >= 1_073_741_824:
		return fmt.Sprintf("%.1fG", float64(bytes)/1_073_741_824)
	case bytes >= 1_048_576:
		return fmt.Sprintf("%.1fM", float64(bytes)/1_048_576)
	case bytes >= 1024:
		return fmt.Sprintf("%.0fK", float64(bytes)/1024)
	default:
		return fmt.Sprintf("%d", bytes)
	}
}
