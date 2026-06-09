package mcp

import (
	"bytes"
	"fmt"
	"strings"
)

const (
	deltaThreshold  = 0.6  // emit delta only when diff < 60% of full content
	deltaMaxLines   = 5000 // skip O(n²) LCS for very large files
	deltaContextLen = 3    // unified-diff context lines on each side of a change
)

// computeLineDelta computes a unified-style line diff between old and new raw file bytes.
// Returns (diff, true) when the diff is compact enough to be worth sending instead of
// the full file. Returns ("", false) when delta is not beneficial.
func computeLineDelta(oldData, newData []byte) (string, bool) {
	if bytes.Equal(oldData, newData) {
		return "", false
	}
	oldLines := splitLines(oldData)
	newLines := splitLines(newData)
	if len(oldLines) > deltaMaxLines || len(newLines) > deltaMaxLines {
		return "", false
	}
	diff := unifiedDiff(oldLines, newLines, deltaContextLen)
	if len(diff) == 0 || len(diff) >= int(deltaThreshold*float64(len(newData))) {
		return "", false
	}
	return diff, true
}

func splitLines(data []byte) []string {
	s := string(data)
	// strip trailing newline so the last element is never an empty ghost line
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

type editOp byte

const (
	opKeep editOp = iota
	opAdd
	opDel
)

type lineEdit struct {
	op   editOp
	line string
}

// lcsEdits returns the minimal edit list (keep/add/del) from old to new using LCS.
func lcsEdits(old, new []string) []lineEdit {
	n, m := len(old), len(new)

	// dp[i][j] = LCS length of old[:i] and new[:j]
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := 1; i <= n; i++ {
		for j := 1; j <= m; j++ {
			if old[i-1] == new[j-1] {
				dp[i][j] = dp[i-1][j-1] + 1
			} else if dp[i-1][j] >= dp[i][j-1] {
				dp[i][j] = dp[i-1][j]
			} else {
				dp[i][j] = dp[i][j-1]
			}
		}
	}

	// backtrack
	edits := make([]lineEdit, 0, n+m)
	i, j := n, m
	for i > 0 || j > 0 {
		switch {
		case i > 0 && j > 0 && old[i-1] == new[j-1]:
			edits = append(edits, lineEdit{opKeep, old[i-1]})
			i--
			j--
		case j > 0 && (i == 0 || dp[i][j-1] >= dp[i-1][j]):
			edits = append(edits, lineEdit{opAdd, new[j-1]})
			j--
		default:
			edits = append(edits, lineEdit{opDel, old[i-1]})
			i--
		}
	}

	// reverse in place
	for l, r := 0, len(edits)-1; l < r; l, r = l+1, r-1 {
		edits[l], edits[r] = edits[r], edits[l]
	}
	return edits
}

type hunk struct {
	oldStart, oldCount int
	newStart, newCount int
	lines              []lineEdit
}

// buildHunks groups edits into unified-diff hunks with ctx lines of context.
func buildHunks(edits []lineEdit, ctx int) []hunk {
	// find indices of changed edits
	changed := make([]bool, len(edits))
	for i, e := range edits {
		changed[i] = e.op != opKeep
	}

	var hunks []hunk
	i := 0
	for i < len(edits) {
		if !changed[i] {
			i++
			continue
		}
		// found a changed edit — expand window by ctx on each side
		start := i - ctx
		if start < 0 {
			start = 0
		}
		// extend end to cover all changes within ctx reach
		end := i + 1
		for end < len(edits) {
			if changed[end] {
				end++
				continue
			}
			// count consecutive keeps after last change
			tail := 0
			for end+tail < len(edits) && !changed[end+tail] {
				tail++
			}
			if tail <= 2*ctx && end+tail < len(edits) {
				// gap small enough to merge with next change block
				end += tail
				continue
			}
			break
		}
		end += ctx
		if end > len(edits) {
			end = len(edits)
		}

		// compute old/new line counters for the hunk header
		oldStart, newStart := 0, 0
		for k := 0; k < start; k++ {
			switch edits[k].op {
			case opKeep:
				oldStart++
				newStart++
			case opDel:
				oldStart++
			case opAdd:
				newStart++
			}
		}
		oldCount, newCount := 0, 0
		slice := edits[start:end]
		for _, e := range slice {
			switch e.op {
			case opKeep:
				oldCount++
				newCount++
			case opDel:
				oldCount++
			case opAdd:
				newCount++
			}
		}

		hunks = append(hunks, hunk{
			oldStart: oldStart,
			oldCount: oldCount,
			newStart: newStart,
			newCount: newCount,
			lines:    slice,
		})
		i = end
	}
	return hunks
}

// unifiedDiff returns a compact unified diff of old vs new with ctx lines of context.
func unifiedDiff(old, new []string, ctx int) string {
	edits := lcsEdits(old, new)
	hunks := buildHunks(edits, ctx)
	if len(hunks) == 0 {
		return ""
	}

	var sb strings.Builder
	for _, h := range hunks {
		fmt.Fprintf(&sb, "@@ -%d,%d +%d,%d @@\n", h.oldStart+1, h.oldCount, h.newStart+1, h.newCount)
		for _, e := range h.lines {
			switch e.op {
			case opKeep:
				sb.WriteByte(' ')
			case opAdd:
				sb.WriteByte('+')
			case opDel:
				sb.WriteByte('-')
			}
			sb.WriteString(e.line)
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}
