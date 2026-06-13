package compress

import (
	"fmt"
	"strings"
)

// ── mix ────────────────────────────────────────────────────────────────────────

func CompressMix(cmd string, lines []string) []string {
	joined := strings.Join(lines, "\n")
	if strings.TrimSpace(joined) == "" {
		return []string{"ok"}
	}
	switch {
	case strings.Contains(cmd, "test"):
		return CompressMixTest(lines)
	case strings.Contains(cmd, "deps.get") || strings.Contains(cmd, "deps.compile"):
		return CompressMixDeps(lines)
	case strings.Contains(cmd, "compile") || strings.Contains(cmd, "build"):
		return CompressMixCompile(lines)
	case strings.Contains(cmd, "format") || strings.Contains(cmd, "fmt"):
		var files []string
		for _, l := range lines {
			if strings.TrimSpace(l) != "" {
				files = append(files, l)
			}
		}
		if len(files) == 0 {
			return []string{"ok (formatted)"}
		}
		return []string{fmt.Sprintf("%d files", len(files))}
	case strings.Contains(cmd, "credo") || strings.Contains(cmd, "dialyzer"):
		return CompressMixLint(lines)
	}
	return CompactLines(lines, 15)
}

func CompressMixTest(lines []string) []string {
	// Find summary line (search from bottom)
	for i := len(lines) - 1; i >= 0; i-- {
		l := lines[i]
		if strings.Contains(l, "test") && (strings.Contains(l, "passed") || strings.Contains(l, "failure")) {
			result := "mix test: " + strings.TrimSpace(l)
			var failures []string
			for _, fl := range lines {
				ft := strings.TrimSpace(fl)
				if len(ft) >= 2 && ft[0] >= '1' && ft[0] <= '9' && ft[1] == ')' {
					failures = append(failures, ft)
				}
			}
			out := []string{result}
			for j, f := range failures {
				if j >= 5 {
					break
				}
				out = append(out, "  "+f)
			}
			return out
		}
	}
	return CompactLines(lines, 10)
}

func CompressMixDeps(lines []string) []string {
	var resolved, compiled int
	for _, l := range lines {
		ll := strings.ToLower(l)
		if strings.Contains(ll, "resolving") {
			resolved++
		}
		if strings.Contains(ll, "compiling") {
			compiled++
		}
	}
	if resolved == 0 && compiled == 0 {
		return CompactLines(lines, 5)
	}
	return []string{fmt.Sprintf("deps: %d resolved, %d compiled", resolved, compiled)}
}

func CompressMixCompile(lines []string) []string {
	var compiled, warnings int
	var errors []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "Compiling") || strings.HasPrefix(t, "Compiled") {
			compiled++
		}
		if strings.Contains(t, "warning:") {
			warnings++
		}
		if strings.Contains(t, "error") && strings.Contains(t, "**") {
			errors = append(errors, t)
		}
	}
	if len(errors) > 0 {
		out := []string{fmt.Sprintf("%d errors", len(errors))}
		for i, e := range errors {
			if i >= 10 {
				break
			}
			out = append(out, "  "+e)
		}
		return out
	}
	result := fmt.Sprintf("%d compiled", compiled)
	if warnings > 0 {
		result += fmt.Sprintf(", %d warnings", warnings)
	}
	return []string{result}
}

func CompressMixLint(lines []string) []string {
	var issues []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.Contains(t, "┃") || strings.HasPrefix(t, "warning:") || strings.HasPrefix(t, "error:") {
			issues = append(issues, t)
		}
	}
	if len(issues) == 0 {
		joined := strings.Join(lines, "\n")
		if strings.Contains(joined, "no issues") || strings.Contains(joined, "Analysis finished") {
			return []string{"clean"}
		}
		return CompactLines(lines, 10)
	}
	out := []string{fmt.Sprintf("%d issues:", len(issues))}
	for i, issue := range issues {
		if i >= 10 {
			break
		}
		out = append(out, "  "+issue)
	}
	return out
}
