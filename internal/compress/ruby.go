package compress

import (
	"fmt"
	"strings"
)

// ── ruby / rubocop ────────────────────────────────────────────────────────────

// CopEntry holds aggregated rubocop offense data for a single cop.
type CopEntry struct {
	name  string
	count int
	files []string
}

func CompressRuby(cmd string, lines []string) []string {
	switch {
	case strings.Contains(cmd, "rubocop"):
		return CompressRubocop(lines)
	case strings.Contains(cmd, "bundle"):
		return CompressBundle(lines)
	case strings.Contains(cmd, "minitest") || strings.Contains(cmd, "test") || strings.Contains(cmd, "rspec"):
		return CompressMinitest(lines)
	}
	return CompactLines(lines, 20)
}

func CompressRubocop(lines []string) []string {
	cops := make(map[string]*CopEntry)
	var copOrder []string
	var summaryLine string

	for _, l := range lines {
		t := strings.TrimSpace(l)
		// Summary line like "2 files inspected, 3 offenses detected"
		if strings.Contains(t, "offense") || strings.Contains(t, "file") && strings.Contains(t, "inspected") {
			if strings.Contains(t, "inspected") {
				summaryLine = t
			}
		}
		// Offense line: "path/file.rb:10:5: C: CopName: message"
		parts := strings.SplitN(t, ": ", 3)
		if len(parts) >= 3 {
			// parts[0] = "file:line:col", parts[1] = "Severity", parts[2] = "CopName: msg"
			severity := strings.TrimSpace(parts[1])
			if severity == "C" || severity == "W" || severity == "E" || severity == "F" {
				copMsg := parts[2]
				copName := copMsg
				if idx := strings.Index(copMsg, ":"); idx > 0 {
					copName = copMsg[:idx]
				}
				file := strings.SplitN(parts[0], ":", 2)[0]
				if _, ok := cops[copName]; !ok {
					cops[copName] = &CopEntry{name: copName}
					copOrder = append(copOrder, copName)
				}
				cops[copName].count++
				if len(cops[copName].files) < 3 {
					found := false
					for _, f := range cops[copName].files {
						if f == file {
							found = true
							break
						}
					}
					if !found {
						cops[copName].files = append(cops[copName].files, file)
					}
				}
			}
		}
	}
	if len(cops) == 0 {
		return lines
	}
	var out []string
	if summaryLine != "" {
		out = append(out, summaryLine)
	}
	grouped := GroupByCop(cops, copOrder)
	out = append(out, grouped...)
	return out
}

// GroupByCop formats rubocop cop entries into summary lines.
func GroupByCop(cops map[string]*CopEntry, order []string) []string {
	var out []string
	for _, name := range order {
		c := cops[name]
		files := strings.Join(c.files, ", ")
		out = append(out, fmt.Sprintf("  %s (%d): %s", c.name, c.count, files))
	}
	return out
}

func CompressBundle(lines []string) []string {
	var installed, using int
	var errors []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "Installing") {
			installed++
		} else if strings.HasPrefix(t, "Using") {
			using++
		} else if strings.HasPrefix(t, "Gem::") || strings.HasPrefix(t, "ERROR:") {
			errors = append(errors, t)
		}
	}
	if installed == 0 && using == 0 && len(errors) == 0 {
		return lines
	}
	result := fmt.Sprintf("bundle: %d installing, %d using", installed, using)
	out := []string{result}
	for i, e := range errors {
		if i >= 5 {
			break
		}
		out = append(out, "  "+e)
	}
	return out
}

// reMinitestHeader matches a minitest failure/error header. Minitest prints
// `FAIL:`/`ERROR:` lines, numbered `1) Failure:` / `1) Error:` blocks, or bare
// `Failure:` / `Error:` headers; all are followed by the test-location line and
// the `Expected: … Actual: …` diff + backtrace, which are flush-left (not
// indented) and run until the next blank line.
var reMinitestHeader = &lazyRe{pattern: `^(FAIL:|ERROR:|(\d+\)\s+)?(Failure|Error):)`}

func CompressMinitest(lines []string) []string {
	var failures, errors int
	var summary string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.Contains(t, "runs,") && strings.Contains(t, "assertions") {
			summary = t
		}
		if strings.HasPrefix(t, "FAIL:") || strings.Contains(t, ") Failure:") {
			failures++
		} else if strings.HasPrefix(t, "ERROR:") || strings.Contains(t, ") Error:") {
			errors++
		}
	}
	// Retain the per-failure diff/backtrace (#455), not just the header line.
	// Minitest detail is flush-left, so any non-blank line is detail until the
	// blank-line boundary that closes the block.
	anyNonBlank := func(l string) bool { return strings.TrimSpace(l) != "" }
	blocks := collectFailures(lines, reMinitestHeader.MatchString, anyNonBlank, 12)
	if summary == "" && failures == 0 && errors == 0 {
		return lines
	}
	var out []string
	if summary != "" {
		out = append(out, summary)
	} else {
		out = append(out, fmt.Sprintf("%d failures, %d errors", failures, errors))
	}
	return appendFailureBlocks(out, blocks, "  ", 5)
}
