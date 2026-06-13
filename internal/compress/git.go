package compress

import (
	"fmt"
	"strings"
)

// ── git ──────────────────────────────────────────────────────────────────────

var (
	reGitDiffHunkParse = &lazyRe{pattern: `^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@`}
)

func CompressGit(lines []string) []string {
	if len(lines) < 80 {
		return lines
	}
	// Reformat to compact +N:/−N: notation — much denser than unified diff.
	compact := compactDiff(lines)
	if len(compact) < len(lines) {
		return compact
	}
	// Fallback: strip context lines only.
	var out []string
	for _, l := range lines {
		if len(l) > 0 && l[0] == ' ' {
			continue
		}
		out = append(out, l)
	}
	if len(out) >= len(lines) {
		return lines
	}
	return out
}

// compactDiff reformats unified diff output to lean +N:/−N: notation.
func compactDiff(lines []string) []string {
	var out []string
	var added, removed int
	oldLine, newLine := 0, 0

	for _, l := range lines {
		if len(l) == 0 {
			continue
		}
		switch {
		case strings.HasPrefix(l, "diff ") || strings.HasPrefix(l, "--- ") ||
			strings.HasPrefix(l, "+++ "):
			out = append(out, l)
		case strings.HasPrefix(l, "index ") || strings.HasPrefix(l, "new file") ||
			strings.HasPrefix(l, "deleted file") || strings.HasPrefix(l, "old mode") ||
			strings.HasPrefix(l, "new mode"):
			// skip low-signal metadata
		case reGitDiffHunkParse.MatchString(l):
			m := reGitDiffHunkParse.FindStringSubmatch(l)
			oldLine = mustAtoi(m[1])
			newLine = mustAtoi(m[2])
		case l[0] == '-':
			out = append(out, fmt.Sprintf("-%d: %s", oldLine, l[1:]))
			oldLine++
			removed++
		case l[0] == '+':
			out = append(out, fmt.Sprintf("+%d: %s", newLine, l[1:]))
			newLine++
			added++
		default: // context line (space prefix)
			oldLine++
			newLine++
		}
	}
	out = append(out, fmt.Sprintf("diff +%d/-%d lines", added, removed))
	return out
}

func mustAtoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// ── gh ────────────────────────────────────────────────────────────────────────

var (
	reGhSeparator = &lazyRe{pattern: `^─+$|^═+$`}
	reGhLabel     = &lazyRe{pattern: `^(Labels|Assignees|Projects|Milestone|Reviewers):\s*$`}
)

func CompressGh(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		switch {
		case reGhSeparator.MatchString(strings.TrimSpace(l)):
			// drop decorative separators
		case reGhLabel.MatchString(l):
			// drop empty metadata fields
		default:
			out = append(out, l)
		}
	}
	if len(out) == 0 {
		return lines
	}
	return out
}
