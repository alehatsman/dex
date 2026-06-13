package compress

import (
	"fmt"
	"regexp"
	"strings"
)

// ── kubectl ───────────────────────────────────────────────────────────────────

var (
	reKubectlHealthy  = &lazyRe{pattern: `\s+Running\s+0\s+`}
	reKubectlProgress = &lazyRe{pattern: `^(Waiting for|waiting for|Watching)`}
	reKubectlBoiler   = &lazyRe{pattern: `^(Warning: resource|kubectl\.kubernetes\.io)`}
	reKubectlLogTS    = &lazyRe{pattern: `^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}`}
)

func CompressKubectl(lines []string) []string {
	out := make([]string, 0, len(lines))
	prev := ""
	for _, l := range lines {
		switch {
		case reKubectlHealthy.MatchString(l):
			// drop fully-ready Running pods — keep only problematic ones
		case reKubectlProgress.MatchString(l):
			// drop "Waiting for deployment..." noise
		case reKubectlBoiler.MatchString(l):
			// drop annotation warnings
		default:
			// deduplicate log lines (strip timestamp for comparison)
			key := l
			if loc := reKubectlLogTS.FindStringIndex(l); loc != nil {
				key = strings.TrimSpace(l[loc[1]:])
			}
			if key != "" && key == prev {
				continue
			}
			out = append(out, l)
			prev = key
		}
	}
	if len(out) == 0 {
		return lines
	}
	return out
}

// ── helm ─────────────────────────────────────────────────────────────────────

var (
	reMavenDownload  = regexp.MustCompile(`(?i)(Downloading|Downloaded)\s+from\s+`)
	reMavenProgress  = regexp.MustCompile(`\d+/\d+\s*(KB|MB|B)`)
	reGradleDownload = regexp.MustCompile(`(?i)Download\s+http`)
	reGradleProgress = regexp.MustCompile(`>\s+\d+%`)
	reMavenTestsRun  = regexp.MustCompile(`Tests run:\s*(\d+),\s*Failures:\s*(\d+),\s*Errors:\s*(\d+),\s*Skipped:\s*(\d+)`)
)

func CompressHelm(cmd string, lines []string) []string {
	switch {
	case strings.Contains(cmd, "list") || strings.Contains(cmd, "ls"):
		return CompressHelmList(lines)
	case strings.Contains(cmd, "install") || strings.Contains(cmd, "upgrade"):
		return CompressHelmInstall(lines)
	case strings.Contains(cmd, "status"):
		return CompressHelmStatus(lines)
	case strings.Contains(cmd, "template"):
		return CompressHelmTemplate(lines)
	}
	return CompactLines(lines, 20)
}

func CompressHelmList(lines []string) []string {
	if len(lines) <= 20 {
		return lines
	}
	out := []string{fmt.Sprintf("%d releases:", len(lines)-1)}
	for i, l := range lines {
		if i == 0 {
			out = append(out, l) // header
			continue
		}
		if i >= 21 {
			out = append(out, fmt.Sprintf("  … +%d more", len(lines)-21))
			break
		}
		out = append(out, l)
	}
	return out
}

func CompressHelmInstall(lines []string) []string {
	var statusLines []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "NAME:") || strings.HasPrefix(t, "STATUS:") ||
			strings.HasPrefix(t, "LAST DEPLOYED:") || strings.HasPrefix(t, "NAMESPACE:") ||
			strings.HasPrefix(t, "REVISION:") || strings.Contains(t, "deployed") ||
			strings.Contains(t, "NOTES:") || strings.HasPrefix(t, "Error") {
			statusLines = append(statusLines, l)
		}
	}
	if len(statusLines) == 0 {
		return CompactLines(lines, 15)
	}
	return statusLines
}

func CompressHelmStatus(lines []string) []string {
	var out []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "NAME:") || strings.HasPrefix(t, "STATUS:") ||
			strings.HasPrefix(t, "LAST DEPLOYED:") || strings.HasPrefix(t, "NAMESPACE:") ||
			strings.HasPrefix(t, "REVISION:") || strings.HasPrefix(t, "NOTES:") {
			out = append(out, l)
		}
	}
	if len(out) == 0 {
		return CompactLines(lines, 15)
	}
	return out
}

func CompressHelmTemplate(lines []string) []string {
	// Count Kubernetes resources.
	kindCount := make(map[string]int)
	var kindOrder []string
	for _, l := range lines {
		if strings.HasPrefix(l, "kind:") {
			kind := strings.TrimSpace(strings.TrimPrefix(l, "kind:"))
			if _, ok := kindCount[kind]; !ok {
				kindOrder = append(kindOrder, kind)
			}
			kindCount[kind]++
		}
	}
	if len(kindCount) == 0 {
		return CompactLines(lines, 30)
	}
	out := []string{fmt.Sprintf("%d resources:", len(lines))}
	for _, k := range kindOrder {
		out = append(out, fmt.Sprintf("  %s: %d", k, kindCount[k]))
	}
	return out
}

// ── ansible ───────────────────────────────────────────────────────────────────

func CompressAnsible(lines []string) []string {
	joined := strings.Join(lines, "\n")
	if strings.TrimSpace(joined) == "" {
		return []string{"ok"}
	}
	if strings.Contains(joined, "PLAY RECAP") || strings.Contains(joined, "TASK [") {
		return CompressAnsiblePlaybook(lines)
	}
	return CompressAnsibleTasks(lines)
}

func CompressAnsiblePlaybook(lines []string) []string {
	var recap []string
	var failures []string
	inRecap := false
	for _, l := range lines {
		if strings.Contains(l, "PLAY RECAP") {
			inRecap = true
			continue
		}
		if inRecap {
			if strings.TrimSpace(l) != "" {
				recap = append(recap, l)
			}
			continue
		}
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "fatal:") || strings.HasPrefix(t, "FAILED!") ||
			strings.Contains(t, "UNREACHABLE!") {
			failures = append(failures, t)
		}
	}
	var out []string
	if len(recap) > 0 {
		out = append(out, "PLAY RECAP:")
		out = append(out, recap...)
	}
	if len(failures) > 0 {
		out = append(out, "failures:")
		for i, f := range failures {
			if i >= 5 {
				out = append(out, fmt.Sprintf("  … +%d more", len(failures)-5))
				break
			}
			out = append(out, "  "+f)
		}
	}
	if len(out) == 0 {
		return CompactLines(lines, 20)
	}
	return out
}

func CompressAnsibleTasks(lines []string) []string {
	var ok, changed, failed int
	for _, l := range lines {
		t := strings.ToLower(strings.TrimSpace(l))
		if strings.HasPrefix(t, "ok:") {
			ok++
		} else if strings.HasPrefix(t, "changed:") {
			changed++
		} else if strings.HasPrefix(t, "fatal:") || strings.HasPrefix(t, "failed:") {
			failed++
		}
	}
	if ok == 0 && changed == 0 && failed == 0 {
		return CompactLines(lines, 10)
	}
	result := fmt.Sprintf("ok=%d changed=%d failed=%d", ok, changed, failed)
	return []string{result}
}
