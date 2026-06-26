// Package review parses unified git diffs into per-file, per-hunk structures.
//
// It is the one genuinely new piece behind the `review` verb (#639 / GitHub #65
// Tier S2): every other lane the verb surfaces (callers, tests, docs, churn,
// notes) already ships in dex and is composed on top of these hunks. The parser
// is deliberately self-contained — no git invocation, no store, no model — so it
// can be unit-tested against literal diff text.
package review

import (
	"strconv"
	"strings"
)

// FileDiff is one file's changes within a diff. Status distinguishes the cases
// the symbol lane must treat differently: a deleted file has no current symbols
// to map, a renamed file may carry hunks under its new path.
type FileDiff struct {
	Path    string // new path (b/…); for a pure delete this is the old path
	OldPath string // a/… when it differs from Path (rename), else ""
	Status  string // "added" | "modified" | "deleted" | "renamed"
	Hunks   []Hunk
}

// Hunk is one @@ block. The New* range addresses lines in the post-change file
// (what the symbol lane maps onto current code); the Old* range addresses the
// pre-change file. A count omitted in the header means 1; a count of 0 means the
// change is a pure insertion (new side) or deletion (old side) at that point.
type Hunk struct {
	OldStart int
	OldLines int
	NewStart int
	NewLines int
	Heading  string   // the optional section heading after the closing @@
	Lines    []string // raw body lines incl. leading ' ', '+', '-'
}

// ParseUnified parses the output of `git diff --unified=N` into per-file hunks.
// It is tolerant of the headers git emits that carry no hunk data — `index`,
// `similarity index`, mode changes, binary-file markers — and skips them. A
// malformed hunk header is skipped rather than fatal, so a single odd file never
// sinks the whole review.
func ParseUnified(diff string) []FileDiff {
	var files []FileDiff
	var cur *FileDiff
	var hunk *Hunk

	flushHunk := func() {
		if cur != nil && hunk != nil {
			cur.Hunks = append(cur.Hunks, *hunk)
			hunk = nil
		}
	}
	flushFile := func() {
		flushHunk()
		if cur != nil {
			files = append(files, *cur)
			cur = nil
		}
	}

	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			flushFile()
			cur = &FileDiff{Status: "modified"}
			// "diff --git a/x b/y" — seed Path from the b/ side; the
			// +++/--- lines below refine it (and handle /dev/null).
			if a, b, ok := parseDiffGit(line); ok {
				cur.OldPath = a
				cur.Path = b
			}
		case cur == nil:
			// Preamble before the first file (e.g. a `git log -p` header) — ignore.
			continue
		case strings.HasPrefix(line, "new file mode"):
			cur.Status = "added"
		case strings.HasPrefix(line, "deleted file mode"):
			cur.Status = "deleted"
		case strings.HasPrefix(line, "rename from "):
			cur.Status = "renamed"
			cur.OldPath = strings.TrimPrefix(line, "rename from ")
		case strings.HasPrefix(line, "rename to "):
			cur.Status = "renamed"
			cur.Path = strings.TrimPrefix(line, "rename to ")
		case strings.HasPrefix(line, "--- "):
			if p, ok := parsePathHeader(line, "--- "); ok {
				cur.OldPath = p
			}
		case strings.HasPrefix(line, "+++ "):
			if p, ok := parsePathHeader(line, "+++ "); ok {
				cur.Path = p
			}
		case strings.HasPrefix(line, "@@"):
			flushHunk()
			if h, ok := parseHunkHeader(line); ok {
				hunk = &h
			}
		case hunk != nil && (strings.HasPrefix(line, "+") ||
			strings.HasPrefix(line, "-") || strings.HasPrefix(line, " ")):
			hunk.Lines = append(hunk.Lines, line)
		default:
			// index lines, mode lines, "\ No newline at end of file", binary
			// markers — nothing the symbol lane needs.
		}
	}
	flushFile()

	// A pure rename with no hunks still has OldPath == Path if only one of the
	// rename lines parsed; normalise so OldPath is empty when unchanged.
	for i := range files {
		if files[i].OldPath == files[i].Path {
			files[i].OldPath = ""
		}
		// A delete points Path at /dev/null via the +++ line; fall back to OldPath.
		if files[i].Path == "" {
			files[i].Path = files[i].OldPath
			files[i].OldPath = ""
		}
	}
	return files
}

// parseDiffGit pulls the a/ and b/ paths out of a "diff --git a/x b/y" line.
// It splits on " b/" so paths containing spaces survive (git does not quote the
// common case). Returns ok=false when the shape is unexpected.
func parseDiffGit(line string) (a, b string, ok bool) {
	rest := strings.TrimPrefix(line, "diff --git ")
	i := strings.Index(rest, " b/")
	if i < 0 || !strings.HasPrefix(rest, "a/") {
		return "", "", false
	}
	a = strings.TrimPrefix(rest[:i], "a/")
	b = strings.TrimPrefix(rest[i+1:], "b/")
	return a, b, true
}

// parsePathHeader extracts the path from a "--- a/path" or "+++ b/path" line,
// stripping the a//b/ prefix. A /dev/null marker (add or delete) yields ok=false
// so the caller keeps the path it already has from the other side.
func parsePathHeader(line, prefix string) (string, bool) {
	p := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	// git appends a tab + timestamp on some diffs; drop anything after a tab.
	if i := strings.IndexByte(p, '\t'); i >= 0 {
		p = p[:i]
	}
	if p == "/dev/null" || p == "" {
		return "", false
	}
	p = strings.TrimPrefix(p, "a/")
	p = strings.TrimPrefix(p, "b/")
	return p, true
}

// parseHunkHeader parses "@@ -oldStart,oldLines +newStart,newLines @@ heading".
// Either count may be omitted (defaults to 1). Returns ok=false on a malformed
// header so the caller can skip it without aborting the file.
func parseHunkHeader(line string) (Hunk, bool) {
	// Body after the leading "@@ ".
	rest := strings.TrimPrefix(line, "@@")
	close := strings.Index(rest, "@@")
	if close < 0 {
		return Hunk{}, false
	}
	ranges := strings.Fields(strings.TrimSpace(rest[:close]))
	if len(ranges) != 2 || !strings.HasPrefix(ranges[0], "-") || !strings.HasPrefix(ranges[1], "+") {
		return Hunk{}, false
	}
	oldStart, oldLines, ok1 := parseRange(strings.TrimPrefix(ranges[0], "-"))
	newStart, newLines, ok2 := parseRange(strings.TrimPrefix(ranges[1], "+"))
	if !ok1 || !ok2 {
		return Hunk{}, false
	}
	return Hunk{
		OldStart: oldStart, OldLines: oldLines,
		NewStart: newStart, NewLines: newLines,
		Heading: strings.TrimSpace(rest[close+2:]),
	}, true
}

// parseRange parses "start,count" or bare "start" (count defaults to 1).
func parseRange(s string) (start, count int, ok bool) {
	count = 1
	if i := strings.IndexByte(s, ','); i >= 0 {
		c, err := strconv.Atoi(s[i+1:])
		if err != nil {
			return 0, 0, false
		}
		count = c
		s = s[:i]
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, 0, false
	}
	return n, count, true
}

// TouchedLines returns the post-change line numbers a hunk introduces or
// modifies — the new-side range, clamped to at least the start line so a pure
// deletion (NewLines == 0) still yields the anchor line where code was removed.
// These are the lines the symbol lane samples via ChunkAt.
func (h Hunk) TouchedLines() []int {
	if h.NewLines <= 0 {
		return []int{h.NewStart}
	}
	lines := make([]int, 0, h.NewLines)
	for i := 0; i < h.NewLines; i++ {
		lines = append(lines, h.NewStart+i)
	}
	return lines
}
