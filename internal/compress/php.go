package compress

import (
	"fmt"
	"regexp"
	"strings"
)

// ── composer ──────────────────────────────────────────────────────────────────

func CompressComposer(cmd string, lines []string) []string {
	switch {
	case strings.Contains(cmd, "install") || strings.Contains(cmd, "update") || strings.Contains(cmd, "require"):
		return CompressComposerInstall(lines)
	case strings.Contains(cmd, "outdated"):
		return CompressComposerOutdated(lines)
	}
	return CompactLines(lines, 15)
}

func CompressComposerInstall(lines []string) []string {
	var installed, updated int
	var errors []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		ll := strings.ToLower(t)
		if strings.HasPrefix(ll, "- installing") || strings.HasPrefix(ll, "  - installing") {
			installed++
		} else if strings.HasPrefix(ll, "- updating") || strings.HasPrefix(ll, "  - updating") {
			updated++
		} else if strings.HasPrefix(ll, "[error]") || strings.HasPrefix(ll, "your requirements") {
			errors = append(errors, t)
		}
	}
	if installed == 0 && updated == 0 && len(errors) == 0 {
		return lines
	}
	result := fmt.Sprintf("composer: %d installed, %d updated", installed, updated)
	out := []string{result}
	for i, e := range errors {
		if i >= 5 {
			break
		}
		out = append(out, "  "+e)
	}
	return out
}

func CompressComposerOutdated(lines []string) []string {
	var packages []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t != "" && !strings.HasPrefix(t, "Legend:") && !strings.HasPrefix(t, "Color") {
			packages = append(packages, t)
		}
	}
	if len(packages) == 0 {
		return []string{"up to date"}
	}
	if len(packages) <= 20 {
		return append([]string{fmt.Sprintf("%d outdated packages:", len(packages))}, packages...)
	}
	out := []string{fmt.Sprintf("%d outdated packages (top 20):", len(packages))}
	return append(out, packages[:20]...)
}

// ── artisan ───────────────────────────────────────────────────────────────────

var (
	reArtisanMigStatus  = regexp.MustCompile(`(Ran|Pending)\s+(\S+)`)
	reArtisanRoute      = regexp.MustCompile(`(GET|POST|PUT|PATCH|DELETE|HEAD)\s+(/[^\s]*)\s+.*?(\S+@\S+|\[Closure\])`)
	reArtisanTestResult = regexp.MustCompile(`(\d+) passed(?:,\s*(\d+) failed)?`)
	reArtisanPest       = regexp.MustCompile(`PASS\s+(\d+)\s+tests?\s*,\s*FAIL\s+(\d+)|Tests:\s*(\d+) passed`)
)

func CompressArtisan(cmd string, lines []string) []string {
	switch {
	case strings.Contains(cmd, "migrate"):
		if strings.Contains(cmd, "status") {
			return CompressArtisanMigrateStatus(lines)
		}
		return CompressArtisanMigrate(lines)
	case strings.Contains(cmd, "test"):
		return CompressArtisanTest(lines)
	case strings.Contains(cmd, "route:list"):
		return CompressArtisanRoutes(lines)
	case strings.Contains(cmd, "make:"):
		return CompressArtisanMake(lines)
	case strings.Contains(cmd, "queue:"):
		return CompressArtisanQueue(lines)
	}
	return CompactLines(lines, 15)
}

func CompressArtisanMigrate(lines []string) []string {
	var migrated, rolled int
	for _, l := range lines {
		t := strings.ToLower(strings.TrimSpace(l))
		if strings.Contains(t, "migrating:") || strings.Contains(t, "migrated:") {
			migrated++
		} else if strings.Contains(t, "rolling back:") || strings.Contains(t, "rolled back:") {
			rolled++
		}
	}
	if migrated == 0 && rolled == 0 {
		return CompactLines(lines, 10)
	}
	result := fmt.Sprintf("migrated: %d, rolled back: %d", migrated, rolled)
	return []string{result}
}

func CompressArtisanMigrateStatus(lines []string) []string {
	joined := strings.Join(lines, "\n")
	matches := reArtisanMigStatus.FindAllStringSubmatch(joined, -1)
	if len(matches) == 0 {
		return CompactLines(lines, 10)
	}
	var statuses []string
	for _, m := range matches {
		prefix := "-"
		if m[1] == "Ran" {
			prefix = "+"
		}
		statuses = append(statuses, prefix+" "+strings.TrimSpace(m[2]))
	}
	ran, pending := 0, 0
	for _, s := range statuses {
		if strings.HasPrefix(s, "+") {
			ran++
		} else {
			pending++
		}
	}
	out := []string{fmt.Sprintf("%d ran, %d pending:", ran, pending)}
	shown := statuses
	if len(shown) > 10 {
		shown = shown[len(shown)-10:]
	}
	for _, s := range shown {
		out = append(out, "  "+s)
	}
	if len(statuses) > 10 {
		out = append(out, fmt.Sprintf("  ... +%d more", len(statuses)-10))
	}
	return out
}

func CompressArtisanTest(lines []string) []string {
	var passed, failed int
	var failures []string
	var timeStr string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if m := reArtisanTestResult.FindStringSubmatch(t); m != nil {
			passed = ParseInt(m[1])
			if m[2] != "" {
				failed = ParseInt(m[2])
			}
		}
		if m := reArtisanPest.FindStringSubmatch(t); m != nil {
			if m[3] != "" {
				passed = ParseInt(m[3])
			} else {
				passed = ParseInt(m[1])
				failed = ParseInt(m[2])
			}
		}
		if strings.HasPrefix(t, "FAIL") || strings.HasPrefix(t, "✕") || strings.HasPrefix(t, "×") {
			failures = append(failures, t)
		}
		if strings.Contains(t, "Time:") || strings.Contains(t, "Duration:") {
			timeStr = t
		}
	}
	status := "ok"
	if failed > 0 {
		status = "FAIL"
	}
	result := fmt.Sprintf("%s: %d passed, %d failed", status, passed, failed)
	if timeStr != "" {
		result += fmt.Sprintf(" (%s)", strings.TrimSpace(timeStr))
	}
	out := []string{result}
	for i, f := range failures {
		if i >= 10 {
			break
		}
		out = append(out, "  "+f)
	}
	return out
}

func CompressArtisanRoutes(lines []string) []string {
	joined := strings.Join(lines, "\n")
	matches := reArtisanRoute.FindAllStringSubmatch(joined, -1)
	if len(matches) == 0 {
		return CompactLines(lines, 15)
	}
	out := []string{fmt.Sprintf("%d routes:", len(matches))}
	for i, m := range matches {
		if i >= 20 {
			break
		}
		out = append(out, fmt.Sprintf("  %s %s → %s", m[1], m[2], m[3]))
	}
	if len(matches) > 20 {
		out = append(out, fmt.Sprintf("  ... +%d more", len(matches)-20))
	}
	return out
}

func CompressArtisanMake(lines []string) []string {
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.Contains(t, "created successfully") || strings.Contains(t, ".php") {
			return []string{t}
		}
	}
	return []string{"created"}
}

func CompressArtisanQueue(lines []string) []string {
	var processed, failed int
	var lastJob string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.Contains(t, "Processed") || strings.Contains(t, "[DONE]") {
			processed++
			if parts := strings.Fields(t); len(parts) > 0 {
				lastJob = parts[len(parts)-1]
			}
		}
		if strings.Contains(t, "FAILED") || strings.Contains(t, "[ERROR]") {
			failed++
		}
	}
	if processed == 0 && failed == 0 {
		return CompactLines(lines, 5)
	}
	result := fmt.Sprintf("queue: %d processed", processed)
	if failed > 0 {
		result += fmt.Sprintf(", %d failed", failed)
	}
	if lastJob != "" {
		result += fmt.Sprintf(" (last: %s)", lastJob)
	}
	return []string{result}
}
