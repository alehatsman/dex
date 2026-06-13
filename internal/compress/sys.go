package compress

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/alehatsman/dex/internal/ignore"
)

// ── ps / du / ping ────────────────────────────────────────────────────────────

func CompressPs(lines []string) []string {
	if len(lines) < 2 {
		return nil
	}
	header := lines[0]
	procs := lines[1:]
	var nonEmpty []string
	for _, p := range procs {
		if strings.TrimSpace(p) != "" {
			nonEmpty = append(nonEmpty, p)
		}
	}
	if len(nonEmpty) <= 10 {
		return nil
	}
	var highCPU, highMem []string
	for _, l := range nonEmpty {
		cols := strings.Fields(l)
		if len(cols) >= 4 {
			cpu := ParseFloat(cols[2])
			mem := ParseFloat(cols[3])
			if cpu > 1.0 {
				highCPU = append(highCPU, l)
			}
			if mem > 1.0 {
				highMem = append(highMem, l)
			}
		}
	}
	out := []string{fmt.Sprintf("ps: %d processes", len(nonEmpty)), header}
	if len(highCPU) > 0 {
		out = append(out, fmt.Sprintf("--- high CPU (%d) ---", len(highCPU)))
		for i, l := range highCPU {
			if i >= 15 {
				break
			}
			out = append(out, l)
		}
	}
	if len(highMem) > 0 {
		out = append(out, fmt.Sprintf("--- high MEM (%d) ---", len(highMem)))
		for i, l := range highMem {
			if i >= 15 {
				break
			}
			out = append(out, l)
		}
	}
	return out
}

func CompressDu(lines []string) []string {
	var nonEmpty []string
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			nonEmpty = append(nonEmpty, l)
		}
	}
	if len(nonEmpty) <= 10 {
		return nil
	}
	type entry struct {
		size uint64
		path string
	}
	var entries []entry
	for _, l := range nonEmpty {
		parts := strings.SplitN(l, "\t", 2)
		if len(parts) == 2 {
			entries = append(entries, entry{ParseSizeField(strings.TrimSpace(parts[0])), strings.TrimSpace(parts[1])})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].size > entries[j].size })
	out := []string{fmt.Sprintf("du: %d entries (top 15 by size)", len(nonEmpty))}
	for i, e := range entries {
		if i >= 15 {
			break
		}
		out = append(out, fmt.Sprintf("%s\t%s", FormatDuSize(e.size), e.path))
	}
	return out
}

func CompressPing(lines []string) []string {
	if len(lines) < 3 {
		return nil
	}
	var host, stats, rtt string
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "PING ") || strings.HasPrefix(l, "ping "):
			host = l
		case strings.Contains(l, "packets transmitted") || strings.Contains(l, "packet loss"):
			stats = l
		case strings.Contains(l, "rtt ") || strings.Contains(l, "round-trip"):
			rtt = l
		}
	}
	if stats == "" {
		return nil
	}
	var out []string
	if host != "" {
		out = append(out, host)
	}
	out = append(out, stats)
	if rtt != "" {
		out = append(out, rtt)
	}
	return out
}

// ── systemd ───────────────────────────────────────────────────────────────────

func CompressSystemd(cmd string, lines []string) []string {
	joined := strings.Join(lines, "\n")
	if strings.TrimSpace(joined) == "" {
		return []string{"ok"}
	}
	if strings.HasPrefix(cmd, "systemctl") {
		return CompressSystemctl(cmd, lines)
	}
	if strings.HasPrefix(cmd, "journalctl") {
		return CompressJournal(lines)
	}
	return CompactLines(lines, 15)
}

func CompressSystemctl(cmd string, lines []string) []string {
	if strings.Contains(cmd, "status") {
		return CompressSystemctlStatus(lines)
	}
	if strings.Contains(cmd, "list-units") || strings.Contains(cmd, "list-unit-files") ||
		(!strings.Contains(cmd, "start") && !strings.Contains(cmd, "stop") &&
			!strings.Contains(cmd, "restart") && !strings.Contains(cmd, "enable") &&
			!strings.Contains(cmd, "disable")) {
		return CompressSystemctlList(lines)
	}
	return CompactLines(lines, 10)
}

func CompressSystemctlStatus(lines []string) []string {
	var parts []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "Active:") || strings.HasPrefix(t, "Loaded:") ||
			strings.HasPrefix(t, "Main PID:") || strings.HasPrefix(t, "Memory:") ||
			strings.HasPrefix(t, "CPU:") || strings.HasPrefix(t, "Tasks:") {
			parts = append(parts, t)
		}
	}
	if len(parts) == 0 {
		return CompactLines(lines, 10)
	}
	return parts
}

func CompressSystemctlList(lines []string) []string {
	var nonEmpty []string
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			nonEmpty = append(nonEmpty, l)
		}
	}
	if len(nonEmpty) <= 20 {
		return nonEmpty
	}
	stateCounts := make(map[string]int)
	for _, l := range nonEmpty[1:] {
		parts := strings.Fields(l)
		if len(parts) >= 3 {
			stateCounts[parts[2]]++
		}
	}
	header := nonEmpty[0]
	out := []string{header, fmt.Sprintf("%d units:", len(nonEmpty)-1)}
	for state, count := range stateCounts {
		out = append(out, fmt.Sprintf("  %s: %d", state, count))
	}
	return out
}

func CompressJournal(lines []string) []string {
	if len(lines) <= 30 {
		return lines
	}
	deduped := make(map[string]int)
	for _, l := range lines {
		// Strip timestamp prefix (first 3 space-separated tokens are timestamp+host+unit)
		parts := strings.SplitN(l, " ", 4)
		key := l
		if len(parts) == 4 {
			key = parts[3]
		}
		deduped[key]++
	}
	type pair struct {
		msg   string
		count int
	}
	var sorted []pair
	for msg, count := range deduped {
		sorted = append(sorted, pair{msg, count})
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].count > sorted[j].count })
	out := []string{fmt.Sprintf("%d log lines (deduped to %d):", len(lines), len(sorted))}
	for i, p := range sorted {
		if i >= 20 {
			break
		}
		if p.count > 1 {
			out = append(out, fmt.Sprintf("  (%dx) %s", p.count, p.msg))
		} else {
			out = append(out, "  "+p.msg)
		}
	}
	return out
}

// ── ls ────────────────────────────────────────────────────────────────────────

func CompressLs(lines []string) []string {
	if len(lines) < 5 {
		return nil
	}
	isLong := false
	for _, l := range lines {
		if strings.HasPrefix(l, "-") || strings.HasPrefix(l, "d") ||
			strings.HasPrefix(l, "l") || strings.HasPrefix(l, "total ") {
			isLong = true
			break
		}
	}
	if isLong {
		return CompressLsLong(lines)
	}
	return CompressLsShort(lines)
}

func CompressLsLong(lines []string) []string {
	var dirs, files []string
	for _, l := range lines {
		if strings.HasPrefix(l, "total ") || strings.TrimSpace(l) == "" {
			continue
		}
		parts := strings.Fields(l)
		if len(parts) < 9 {
			continue
		}
		name := strings.Join(parts[8:], " ")
		if name == "." || name == ".." {
			continue
		}
		if strings.HasPrefix(l, "d") {
			dirs = append(dirs, name+"/")
		} else {
			size := LsFormatSize(parts[4])
			files = append(files, fmt.Sprintf("%s  %s", name, size))
		}
	}
	if len(dirs) == 0 && len(files) == 0 {
		return nil
	}
	var out []string
	out = append(out, dirs...)
	out = append(out, files...)
	out = append(out, "")
	out = append(out, fmt.Sprintf("%d files, %d dirs", len(files), len(dirs)))
	return out
}

func CompressLsShort(lines []string) []string {
	var items []string
	for _, l := range lines {
		for _, w := range strings.Fields(l) {
			if w != "" {
				items = append(items, w)
			}
		}
	}
	if len(items) < 10 {
		return nil
	}
	var dirs, files []string
	for _, item := range items {
		if strings.HasSuffix(item, "/") {
			dirs = append(dirs, item)
		} else {
			files = append(files, item)
		}
	}
	var out []string
	out = append(out, dirs...)
	// wrap files at 70 chars
	var lineBuf strings.Builder
	for _, f := range files {
		if lineBuf.Len()+len(f)+2 > 70 {
			out = append(out, lineBuf.String())
			lineBuf.Reset()
		}
		if lineBuf.Len() > 0 {
			lineBuf.WriteString("  ")
		}
		lineBuf.WriteString(f)
	}
	if lineBuf.Len() > 0 {
		out = append(out, lineBuf.String())
	}
	out = append(out, "")
	out = append(out, fmt.Sprintf("%d items", len(dirs)+len(files)))
	return out
}

func LsFormatSize(sizeStr string) string {
	if len(sizeStr) == 0 {
		return "0B"
	}
	last := sizeStr[len(sizeStr)-1]
	if last == 'K' || last == 'M' || last == 'G' || last == 'T' {
		return sizeStr
	}
	n := ParseUint64(sizeStr)
	switch {
	case n >= 1_048_576:
		return fmt.Sprintf("%.1fM", float64(n)/1_048_576)
	case n >= 1024:
		return fmt.Sprintf("%.1fK", float64(n)/1024)
	default:
		return fmt.Sprintf("%dB", n)
	}
}

// ── env filter ────────────────────────────────────────────────────────────────

var envSensitivePatterns = []string{
	"KEY", "SECRET", "TOKEN", "PASSWORD", "PASSWD", "PASSPHRASE", "CREDENTIALS",
	"AUTH", "API_KEY", "PRIVATE", "CERT", "DSN", "CONNECTION", "SALT",
	"SESSION", "PEM", "SIGNING", "ENCRYPTION",
}

// envCredURLRe matches the credentials in a scheme://user:password@host URL
// (postgres://, redis://, amqp://, mongodb://, …). The username may be empty
// (redis://:pw@host). It captures the leading scheme://user: span and the
// trailing @ so ReplaceAllString can redact only the password, keeping the
// rest of the connection string useful (#460).
var envCredURLRe = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://[^:/?#\s]*:)[^@/?#\s]+(@)`)

func CompressEnvFilter(lines []string) []string {
	if len(lines) == 0 {
		return []string{"(empty)"}
	}
	groups := make(map[string][]string)
	var groupOrder []string
	var ungrouped []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" {
			continue
		}
		if idx := strings.Index(t, "="); idx > 0 { // idx>0 intentional: rejects "=VALUE" (empty key)
			key := t[:idx]
			value := t[idx+1:]
			isSensitive := false
			keyUpper := strings.ToUpper(key)
			for _, p := range envSensitivePatterns {
				if strings.Contains(keyUpper, p) {
					isSensitive = true
					break
				}
			}
			// Mask by key name, then fall back to value-based detection so a
			// secret under an innocuous key still gets redacted: a value that
			// is itself a recognizable secret token (LooksLikeSecret), and the
			// password inside a scheme://user:pass@host connection URL — e.g.
			// DATABASE_URL=postgres://u:pw@h leaks pw despite a benign key
			// (#460). Redaction runs before length truncation so a long
			// secret can never be partially exposed by the [:40] cut.
			displayVal := value
			switch {
			case isSensitive || ignore.LooksLikeSecret([]byte(value)):
				displayVal = "***"
			default:
				displayVal = envCredURLRe.ReplaceAllString(value, "${1}***${2}")
				if len(displayVal) > 80 {
					displayVal = displayVal[:40] + "..."
				}
			}
			entry := key + "=" + displayVal
			prefix := key
			if idx2 := strings.Index(key, "_"); idx2 > 0 {
				prefix = key[:idx2]
			}
			if _, exists := groups[prefix]; !exists {
				groupOrder = append(groupOrder, prefix)
			}
			groups[prefix] = append(groups[prefix], entry)
		} else {
			ungrouped = append(ungrouped, t)
		}
	}
	total := 0
	for _, v := range groups {
		total += len(v)
	}
	total += len(ungrouped)
	out := []string{fmt.Sprintf("%d variables:", total)}
	for _, prefix := range groupOrder {
		vars := groups[prefix]
		if len(vars) >= 3 {
			out = append(out, fmt.Sprintf("[%s_*] (%d vars)", prefix, len(vars)))
			for i, v := range vars {
				if i >= 3 {
					break
				}
				out = append(out, "  "+v)
			}
			if len(vars) > 3 {
				out = append(out, fmt.Sprintf("  ... +%d more", len(vars)-3))
			}
		}
	}
	// collect small groups
	var small []string
	for _, prefix := range groupOrder {
		if len(groups[prefix]) < 3 {
			small = append(small, groups[prefix]...)
		}
	}
	small = append(small, ungrouped...)
	if len(small) > 0 {
		out = append(out, fmt.Sprintf("[other] (%d vars)", len(small)))
		for i, v := range small {
			if i >= 5 {
				break
			}
			out = append(out, "  "+v)
		}
		if len(small) > 5 {
			out = append(out, fmt.Sprintf("  ... +%d more", len(small)-5))
		}
	}
	return out
}
