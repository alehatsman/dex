package compress

import (
	"fmt"
	"strings"
)

// ── mysql / psql ──────────────────────────────────────────────────────────────

func CompressMySQL(cmd string, lines []string) []string {
	joined := strings.TrimSpace(strings.Join(lines, "\n"))
	if joined == "" {
		return []string{"ok"}
	}
	if IsMySQLTableOutput(lines) {
		return CompressMySQLTable(lines)
	}
	if strings.Contains(cmd, "show databases") || strings.Contains(cmd, "show tables") {
		return CompressMySQLShow(lines)
	}
	if strings.HasPrefix(joined, "Query OK") || strings.HasPrefix(joined, "Empty set") {
		return []string{strings.Split(joined, "\n")[0]}
	}
	return CompactLines(lines, 20)
}

func IsMySQLTableOutput(lines []string) bool {
	for _, l := range lines {
		if strings.HasPrefix(l, "+") && strings.Contains(l, "---") {
			return true
		}
	}
	return false
}

func CompressMySQLTable(lines []string) []string {
	var dataLines []string
	for _, l := range lines {
		if !strings.HasPrefix(l, "+") && strings.TrimSpace(l) != "" {
			dataLines = append(dataLines, l)
		}
	}
	rowCount := 0
	if len(dataLines) > 1 {
		rowCount = len(dataLines) - 1
	}
	if rowCount <= 20 {
		return lines
	}
	// Find second separator row (end of header)
	sepCount := 0
	headerEnd := 3
	for i, l := range lines {
		if strings.HasPrefix(l, "+") {
			sepCount++
			if sepCount == 2 {
				headerEnd = i + 1
				break
			}
		}
	}
	previewEnd := headerEnd + 10
	if previewEnd > len(lines) {
		previewEnd = len(lines)
	}
	// Copy before append: lines[:previewEnd] keeps lines' backing array
	// (cap > previewEnd here), so appending the summary would overwrite
	// lines[previewEnd] in place and return a slice aliased to the caller's
	// input (#459, follow-up to #454).
	out := append([]string(nil), lines[:previewEnd]...)
	return append(out, fmt.Sprintf("... (%d rows total)", rowCount))
}

func CompressMySQLShow(lines []string) []string {
	var items []string
	for _, l := range lines {
		t := strings.TrimSpace(strings.Trim(l, "|"))
		t = strings.TrimSpace(t)
		if t == "" || strings.HasPrefix(l, "+") || strings.Contains(t, "---") ||
			t == "Database" || strings.HasPrefix(t, "Tables_in") {
			continue
		}
		items = append(items, t)
	}
	if len(items) == 0 {
		return []string{"empty"}
	}
	if len(items) <= 30 {
		return []string{fmt.Sprintf("%d items: %s", len(items), strings.Join(items, ", "))}
	}
	return []string{fmt.Sprintf("%d items: %s, ... +%d more",
		len(items), strings.Join(items[:20], ", "), len(items)-20)}
}

func CompressPsql(cmd string, lines []string) []string {
	joined := strings.TrimSpace(strings.Join(lines, "\n"))
	if joined == "" {
		return []string{"ok"}
	}
	if IsPsqlTableOutput(lines) {
		return CompressPsqlTable(lines)
	}
	if strings.Contains(cmd, `\dt`) || strings.Contains(cmd, `\d`) {
		return CompressPsqlDescribe(lines)
	}
	for _, prefix := range []string{"INSERT", "UPDATE", "DELETE", "CREATE", "ALTER", "DROP"} {
		if strings.HasPrefix(joined, prefix) {
			return []string{strings.Split(joined, "\n")[0]}
		}
	}
	return CompactLines(lines, 20)
}

func IsPsqlTableOutput(lines []string) bool {
	for _, l := range lines {
		if strings.Contains(l, "---+---") || strings.Contains(l, "-+-") {
			return true
		}
	}
	return false
}

func CompressPsqlTable(lines []string) []string {
	sepIdx := 0
	for i, l := range lines {
		if strings.Contains(l, "---+---") || strings.Contains(l, "-+-") {
			sepIdx = i
			break
		}
	}
	var dataRows int
	for _, l := range lines[sepIdx+1:] {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "(") {
			continue
		}
		dataRows++
	}
	// Find row count line like "(42 rows)"
	var countStr string
	for i := len(lines) - 1; i >= 0; i-- {
		t := strings.TrimSpace(lines[i])
		if strings.HasPrefix(t, "(") && strings.Contains(t, "row") {
			countStr = t
			break
		}
	}
	if countStr == "" {
		countStr = fmt.Sprintf("(%d rows)", dataRows)
	}
	if dataRows <= 20 {
		return lines
	}
	previewEnd := sepIdx + 11
	if previewEnd > len(lines) {
		previewEnd = len(lines)
	}
	// Copy before append — see CompressMySQLTable above (#459).
	out := append([]string(nil), lines[:previewEnd]...)
	return append(out, "... "+countStr)
}

func CompressPsqlDescribe(lines []string) []string {
	var nonEmpty []string
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			nonEmpty = append(nonEmpty, l)
		}
	}
	if len(nonEmpty) <= 30 {
		return nonEmpty
	}
	out := nonEmpty[:20]
	return append(out, fmt.Sprintf("... (%d more lines)", len(nonEmpty)-20))
}
