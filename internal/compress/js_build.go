package compress

import (
	"fmt"
	"strings"
)

// ── next build / vite ─────────────────────────────────────────────────────────

var (
	reNextRoute     = &lazyRe{pattern: `^[┌├└─│]\s+(○|●|ƒ|λ|◐)\s+(/[^\s]*)(\s+[\d.]+\s*\w+)?`}
	reNextSize      = &lazyRe{pattern: `First Load JS`}
	reNextBuildTime = &lazyRe{pattern: `Compiled in|compiled in|Build time`}
	reViteChunk     = &lazyRe{pattern: `^(dist/|\.\/dist/).*\s+[\d.]+\s*(kB|B|MB)`}
)

func CompressNextBuild(cmd string, lines []string) []string {
	if strings.Contains(cmd, "vite") {
		return CompressViteBuild(lines)
	}
	// Check for build error.
	for _, l := range lines {
		if strings.Contains(l, "Failed to compile") || strings.Contains(l, "Build error") {
			var errors []string
			capturing := false
			for _, el := range lines {
				if strings.Contains(el, "Failed to compile") || strings.Contains(el, "Build error") {
					capturing = true
					errors = append(errors, "BUILD ERROR: "+strings.TrimSpace(el))
					continue
				}
				if capturing {
					t := strings.TrimSpace(el)
					if t != "" {
						errors = append(errors, "  "+t)
					}
				}
			}
			return errors
		}
	}

	// Extract routes.
	var routes []string
	var buildTime string
	for _, l := range lines {
		if reNextRoute.MatchString(l) {
			routes = append(routes, strings.TrimSpace(l))
		}
		if reNextBuildTime.MatchString(l) {
			buildTime = strings.TrimSpace(l)
		}
	}

	if len(routes) == 0 {
		return CompactLines(lines, 20)
	}

	out := []string{fmt.Sprintf("routes: %d", len(routes))}
	for i, r := range routes {
		if i >= 20 {
			out = append(out, fmt.Sprintf("  … +%d more", len(routes)-20))
			break
		}
		out = append(out, "  "+r)
	}
	if buildTime != "" {
		out = append(out, buildTime)
	}
	_ = reNextSize
	return out
}

func CompressViteBuild(lines []string) []string {
	var chunks []string
	var buildTime string
	modules := 0
	for _, l := range lines {
		if reViteChunk.MatchString(l) {
			chunks = append(chunks, strings.TrimSpace(l))
		}
		if strings.Contains(l, "modules transformed") {
			if n := extractFirstInt(l); n > 0 {
				modules = n
			}
		}
		if strings.Contains(l, "built in") {
			buildTime = strings.TrimSpace(l)
		}
	}
	if len(chunks) == 0 {
		return CompactLines(lines, 20)
	}
	header := "built"
	if modules > 0 {
		header = fmt.Sprintf("built (%d modules)", modules)
	}
	if buildTime != "" {
		header += " — " + buildTime
	}
	out := []string{header, fmt.Sprintf("chunks: %d", len(chunks))}
	for i, c := range chunks {
		if i >= 10 {
			out = append(out, fmt.Sprintf("  … +%d more", len(chunks)-10))
			break
		}
		out = append(out, "  "+c)
	}
	return out
}
