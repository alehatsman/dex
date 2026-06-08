package mcp

// Extended shell-output compression patterns ported from lean-ctx.
// Each function follows the same contract as server_compress.go:
//   compressX(lines []string) []string   or
//   compressX(cmd string, lines []string) []string
// Returns nil/empty to signal "no improvement", original output then used.

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ── helm ─────────────────────────────────────────────────────────────────────

func compressHelm(cmd string, lines []string) []string {
	joined := strings.Join(lines, "\n")
	if strings.TrimSpace(joined) == "" {
		return []string{"ok"}
	}
	switch {
	case strings.Contains(cmd, "list") || strings.Contains(cmd, " ls"):
		return compressHelmList(lines)
	case strings.Contains(cmd, "install") || strings.Contains(cmd, "upgrade"):
		return compressHelmInstall(lines)
	case strings.Contains(cmd, "status"):
		return compressHelmStatus(lines)
	case strings.Contains(cmd, "template") || strings.Contains(cmd, "dry-run"):
		return compressHelmTemplate(lines)
	}
	return compactLines(lines, 15)
}

func compressHelmList(lines []string) []string {
	var nonEmpty []string
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			nonEmpty = append(nonEmpty, l)
		}
	}
	if len(nonEmpty) <= 1 {
		return []string{"no releases"}
	}
	if len(nonEmpty) <= 15 {
		return nonEmpty
	}
	header := nonEmpty[0]
	releases := nonEmpty[1:]
	out := []string{header}
	out = append(out, releases[:10]...)
	out = append(out, fmt.Sprintf("... (%d more)", len(releases)-10))
	return out
}

func compressHelmInstall(lines []string) []string {
	var name, status string
	var notesStart bool
	var notes []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "NAME:") {
			name = strings.TrimSpace(strings.TrimPrefix(t, "NAME:"))
		} else if strings.HasPrefix(t, "STATUS:") {
			status = strings.TrimSpace(strings.TrimPrefix(t, "STATUS:"))
		} else if t == "NOTES:" {
			notesStart = true
		} else if notesStart && t != "" && len(notes) < 5 {
			notes = append(notes, t)
		}
	}
	result := fmt.Sprintf("%s: %s", name, status)
	if len(notes) > 0 {
		result += "\nnotes: " + strings.Join(notes, " | ")
	}
	if name == "" && status == "" {
		return compactLines(lines, 15)
	}
	return strings.Split(result, "\n")
}

func compressHelmStatus(lines []string) []string {
	var parts []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "NAME:") || strings.HasPrefix(t, "STATUS:") ||
			strings.HasPrefix(t, "NAMESPACE:") || strings.HasPrefix(t, "REVISION:") ||
			strings.HasPrefix(t, "LAST DEPLOYED:") {
			parts = append(parts, t)
		}
	}
	if len(parts) == 0 {
		return compactLines(lines, 10)
	}
	return parts
}

func compressHelmTemplate(lines []string) []string {
	var kinds []string
	var docCount int
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t == "---" {
			docCount++
		}
		if strings.HasPrefix(t, "kind:") {
			kind := strings.TrimSpace(strings.TrimPrefix(t, "kind:"))
			kinds = append(kinds, kind)
		}
	}
	if len(kinds) == 0 {
		return []string{fmt.Sprintf("%d lines of YAML", len(lines))}
	}
	counts := make(map[string]int)
	for _, k := range kinds {
		counts[k]++
	}
	if docCount == 0 {
		docCount = 1
	}
	out := []string{fmt.Sprintf("%d YAML docs (%d resources):", docCount, len(kinds))}
	for k, v := range counts {
		out = append(out, fmt.Sprintf("  %s: %d", k, v))
	}
	return out
}

// ── ansible ───────────────────────────────────────────────────────────────────

func compressAnsible(lines []string) []string {
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "PLAY RECAP") {
		return compressAnsiblePlaybook(lines)
	}
	if strings.Contains(joined, "TASK [") {
		return compressAnsibleTasks(lines)
	}
	return compactLines(lines, 15)
}

func compressAnsiblePlaybook(lines []string) []string {
	var recap []string
	inRecap := false
	for _, l := range lines {
		if strings.Contains(l, "PLAY RECAP") {
			inRecap = true
			continue
		}
		if inRecap {
			t := strings.TrimSpace(l)
			if t != "" {
				recap = append(recap, "  "+t)
			}
		}
	}
	if len(recap) == 0 {
		return compactLines(lines, 15)
	}
	return append([]string{"PLAY RECAP:"}, recap...)
}

func compressAnsibleTasks(lines []string) []string {
	counts := make(map[string]int)
	for _, l := range lines {
		t := strings.TrimSpace(l)
		for _, status := range []string{"ok", "changed", "failed", "skipping"} {
			if strings.HasPrefix(t, status+":") {
				counts[status]++
				break
			}
		}
	}
	if len(counts) == 0 {
		return compactLines(lines, 15)
	}
	var parts []string
	for status, n := range counts {
		parts = append(parts, fmt.Sprintf("%s: %d", status, n))
	}
	return []string{strings.Join(parts, ", ")}
}

// ── maven / gradle ────────────────────────────────────────────────────────────

var (
	reMavenDownload = regexp.MustCompile(`(?i)\[INFO\]\s+(Downloading|Downloaded)\s+`)
	reMavenProgress = regexp.MustCompile(`\[INFO\].*kB\s+\|`)
	reGradleDownload = regexp.MustCompile(`(?i)^(Downloading|Download)\s+https?://`)
	reGradleProgress = regexp.MustCompile(`^[<>=\s]+$|^[0-9]+%\s+EXECUTING`)
	reMavenTestsRun  = regexp.MustCompile(`Tests run:\s*\d+`)
)

func isMavenNoise(line string) bool {
	t := strings.TrimLeft(line, " \t")
	if reMavenDownload.MatchString(t) || reMavenProgress.MatchString(t) {
		return true
	}
	return strings.Contains(t, "Progress (") && strings.Contains(t, "):") && strings.Contains(t, "%")
}

func isGradleNoise(line string) bool {
	t := strings.TrimSpace(line)
	if reGradleDownload.MatchString(t) || reGradleProgress.MatchString(t) {
		return true
	}
	tl := strings.ToLower(t)
	return strings.HasPrefix(tl, "consider enabling configuration cache") ||
		strings.Contains(tl, "deprecated gradle features were used") ||
		strings.HasPrefix(tl, "you can use '--warning-mode")
}

func compressMaven(cmd string, lines []string) []string {
	isGradle := strings.HasPrefix(cmd, "gradle ") || strings.HasPrefix(cmd, "./gradlew ") ||
		strings.HasPrefix(cmd, "gradlew ")
	if isGradle {
		return compressGradle(lines)
	}
	var kept []string
	for _, l := range lines {
		t := strings.TrimRight(l, " \t")
		if strings.TrimSpace(t) == "" || isMavenNoise(t) {
			continue
		}
		tl := strings.ToLower(t)
		if strings.Contains(tl, "[error]") || strings.Contains(tl, "[fatal]") ||
			strings.Contains(tl, "build failure") || strings.Contains(tl, "build success") ||
			strings.Contains(tl, "failure!") || strings.Contains(tl, "tests run:") ||
			strings.Contains(tl, "failures:") || strings.Contains(tl, "errors:") ||
			strings.Contains(tl, "skipped:") || reMavenTestsRun.MatchString(t) ||
			strings.Contains(tl, "[warning]") {
			kept = append(kept, strings.TrimSpace(t))
		}
	}
	if len(kept) == 0 {
		return []string{"mvn (no build/test lines kept)"}
	}
	return kept
}

func compressGradle(lines []string) []string {
	var kept, taskLines []string
	for _, l := range lines {
		t := strings.TrimRight(l, " \t")
		if strings.TrimSpace(t) == "" || isGradleNoise(t) {
			continue
		}
		tl := strings.ToLower(t)
		if strings.HasPrefix(tl, "> task ") {
			if strings.Contains(tl, "failed") || strings.Contains(tl, "failure") ||
				strings.Contains(tl, "skipped") || strings.Contains(tl, "no-source") {
				taskLines = append(taskLines, strings.TrimSpace(t))
			}
			continue
		}
		if strings.Contains(tl, "actionable tasks:") || strings.Contains(tl, "build successful") ||
			strings.Contains(tl, "build failed") || strings.HasPrefix(tl, "failure:") ||
			strings.Contains(tl, "what went wrong:") || strings.Contains(tl, "execution failed for task") ||
			strings.Contains(tl, "error:") || strings.Contains(tl, "exception") ||
			strings.Contains(tl, "tests completed:") ||
			(strings.Contains(tl, "test ") && (strings.Contains(tl, "failed") || strings.Contains(tl, "passed"))) ||
			strings.Contains(tl, "there were failing tests") {
			kept = append(kept, strings.TrimSpace(t))
		}
	}
	if len(taskLines) > 0 {
		kept = append(kept, "tasks:")
		kept = append(kept, taskLines...)
	}
	if len(kept) == 0 {
		return []string{"gradle (no summary kept)"}
	}
	return kept
}

// ── bazel ─────────────────────────────────────────────────────────────────────

func compressBazel(cmd string, lines []string) []string {
	joined := strings.Join(lines, "\n")
	if strings.TrimSpace(joined) == "" {
		return []string{"ok"}
	}
	switch {
	case strings.Contains(cmd, "test"):
		return compressBazelTest(lines)
	case strings.Contains(cmd, "build"):
		return compressBazelBuild(lines)
	case strings.Contains(cmd, "query"):
		return compressBazelQuery(lines)
	}
	return compactLines(lines, 15)
}

func compressBazelTest(lines []string) []string {
	var passed, failed int
	var failures []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.Contains(t, "PASSED") {
			passed++
		}
		if strings.Contains(t, "FAILED") {
			failed++
			failures = append(failures, t)
		}
	}
	var summary string
	for _, l := range lines {
		if strings.Contains(l, "executed") || strings.Contains(l, "test(s)") {
			summary = strings.TrimSpace(l)
			break
		}
	}
	if passed == 0 && failed == 0 {
		if summary != "" {
			return []string{"bazel test: " + summary}
		}
		return compactLines(lines, 10)
	}
	result := fmt.Sprintf("bazel test: %d passed", passed)
	if failed > 0 {
		result += fmt.Sprintf(", %d failed", failed)
	}
	out := []string{result}
	for _, f := range failures {
		if len(out) >= 6 {
			break
		}
		out = append(out, "  "+f)
	}
	return out
}

func compressBazelBuild(lines []string) []string {
	var errors []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "ERROR:") || strings.HasPrefix(t, "error:") {
			errors = append(errors, t)
		}
	}
	if len(errors) > 0 {
		out := []string{fmt.Sprintf("%d errors:", len(errors))}
		for _, e := range errors {
			if len(out) >= 11 {
				break
			}
			out = append(out, "  "+e)
		}
		return out
	}
	for i := len(lines) - 1; i >= 0; i-- {
		l := lines[i]
		if strings.Contains(l, "INFO: Build completed") || strings.Contains(l, "up-to-date") {
			return []string{strings.TrimSpace(l)}
		}
	}
	return []string{"ok"}
}

func compressBazelQuery(lines []string) []string {
	var targets []string
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			targets = append(targets, l)
		}
	}
	if len(targets) <= 20 {
		return targets
	}
	out := []string{fmt.Sprintf("%d targets:", len(targets))}
	out = append(out, targets[:15]...)
	out = append(out, fmt.Sprintf("... (%d more)", len(targets)-15))
	return out
}

// ── poetry ────────────────────────────────────────────────────────────────────

var (
	rePoetryInstalling = regexp.MustCompile(`(?i)^\s*-\s+Installing\s+(\S+)\s+\(([^)]+)\)`)
	rePoetryUpdating   = regexp.MustCompile(`(?i)^\s*-\s+Updating\s+(\S+)\s+\(([^)]+)\)`)
	rePoetryPercentBar = regexp.MustCompile(`\d+%\|`)
)

func isPoetryDownloadNoise(line string) bool {
	t := strings.TrimSpace(line)
	tl := strings.ToLower(t)
	if strings.Contains(tl, "downloading ") || strings.HasPrefix(tl, "downloading [") ||
		strings.Contains(tl, "kib/s") || strings.Contains(tl, "mib/s") {
		return true
	}
	if strings.Contains(t, "%") && (strings.Contains(tl, "eta") || strings.Contains(t, "|") || strings.Contains(tl, "of ")) {
		return true
	}
	if strings.HasPrefix(tl, "progress ") && strings.Contains(t, "/") {
		return true
	}
	return rePoetryPercentBar.MatchString(t)
}

func compressPoetry(cmd string, lines []string) []string {
	sub := ""
	parts := strings.Fields(cmd)
	if len(parts) >= 2 {
		sub = parts[1]
	}
	preferUpdate := sub == "update"

	var packages, errors []string
	for _, l := range lines {
		t := strings.TrimRight(l, " \t")
		if strings.TrimSpace(t) == "" || isPoetryDownloadNoise(t) {
			continue
		}
		trim := strings.TrimSpace(t)
		tl := strings.ToLower(trim)
		if preferUpdate {
			if m := rePoetryUpdating.FindStringSubmatch(trim); m != nil {
				packages = append(packages, m[1]+" "+m[2])
				continue
			}
		}
		if m := rePoetryInstalling.FindStringSubmatch(trim); m != nil {
			packages = append(packages, m[1]+" "+m[2])
			continue
		}
		if !preferUpdate {
			if m := rePoetryUpdating.FindStringSubmatch(trim); m != nil {
				packages = append(packages, m[1]+" "+m[2])
				continue
			}
		}
		if strings.Contains(tl, "error") &&
			(strings.Contains(tl, "because") || strings.Contains(tl, "could not") || strings.Contains(tl, "failed")) {
			errors = append(errors, trim)
		}
	}
	var out []string
	if len(packages) > 0 {
		out = append(out, fmt.Sprintf("%d package(s):", len(packages)))
		for _, p := range packages {
			out = append(out, "  "+p)
		}
	}
	if len(errors) > 0 {
		out = append(out, fmt.Sprintf("%d error line(s):", len(errors)))
		for i, e := range errors {
			if i >= 15 {
				break
			}
			out = append(out, "  "+e)
		}
	}
	if len(out) == 0 {
		return compactLinesFiltered(lines, 12)
	}
	return out
}

// ── prisma ────────────────────────────────────────────────────────────────────

var rePrismaBlockChars = regexp.MustCompile(`[█▀━▄▌▐]`)

func compressPrisma(cmd string, lines []string) []string {
	joined := strings.Join(lines, "\n")
	if strings.TrimSpace(joined) == "" {
		return []string{"ok"}
	}
	switch {
	case strings.Contains(cmd, "generate"):
		return compressPrismaGenerate(lines)
	case strings.Contains(cmd, "migrate"):
		return compressPrismaMigrate(lines)
	case strings.Contains(cmd, "db push") || strings.Contains(cmd, "db pull"):
		return compressPrismaDbSync(lines)
	case strings.Contains(cmd, "studio"):
		return []string{"Prisma Studio started"}
	case strings.Contains(cmd, "format"):
		if strings.Contains(joined, "already formatted") || strings.Contains(joined, "unchanged") {
			return []string{"ok (already formatted)"}
		}
		return compressPrismaStripNoise(lines)
	case strings.Contains(cmd, "validate"):
		if strings.Contains(joined, "valid") && !strings.Contains(joined, "invalid") {
			return []string{"ok (schema valid)"}
		}
		return compactLines(lines, 10)
	}
	return compactLines(lines, 10)
}

func compressPrismaGenerate(lines []string) []string {
	var generated []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		plain := rePrismaBlockChars.ReplaceAllString(t, "")
		if strings.Contains(plain, "Generated") || strings.Contains(plain, "generated") {
			generated = append(generated, plain)
		}
	}
	if len(generated) == 0 {
		return compressPrismaStripNoise(lines)
	}
	return generated
}

func compressPrismaMigrate(lines []string) []string {
	var results []string
	var migrationName string
	for _, l := range lines {
		plain := strings.TrimSpace(rePrismaBlockChars.ReplaceAllString(l, ""))
		if strings.Contains(plain, "migration") && strings.Contains(plain, "created") {
			migrationName = plain
		}
		if strings.Contains(plain, "applied") || strings.Contains(plain, "Already in sync") ||
			strings.Contains(plain, "Database is up to date") {
			results = append(results, plain)
		}
	}
	if len(results) == 0 && migrationName == "" {
		return compressPrismaStripNoise(lines)
	}
	var out []string
	if migrationName != "" {
		out = append(out, migrationName)
	}
	out = append(out, results...)
	return out
}

func compressPrismaDbSync(lines []string) []string {
	var out []string
	for _, l := range lines {
		plain := strings.TrimSpace(rePrismaBlockChars.ReplaceAllString(l, ""))
		if plain == "" || strings.Contains(strings.ToLower(plain), "warn") ||
			strings.HasPrefix(plain, "Prisma schema") {
			continue
		}
		out = append(out, plain)
	}
	if len(out) == 0 {
		return []string{"ok (synced)"}
	}
	return out
}

func compressPrismaStripNoise(lines []string) []string {
	var out []string
	for _, l := range lines {
		plain := strings.TrimSpace(rePrismaBlockChars.ReplaceAllString(l, ""))
		if plain != "" {
			out = append(out, plain)
		}
	}
	return out
}

// ── prettier ──────────────────────────────────────────────────────────────────

func compressPrettier(lines []string) []string {
	joined := strings.TrimSpace(strings.Join(lines, "\n"))
	if joined == "" {
		return []string{"ok (formatted)"}
	}
	if strings.Contains(joined, "All matched files use Prettier code style") {
		return []string{"ok (all formatted)"}
	}
	var unformatted, warnings []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "Checking") || strings.HasPrefix(t, "All matched") {
			continue
		}
		if strings.Contains(t, "[warn]") {
			warnings = append(warnings, t)
			continue
		}
		if !strings.Contains(t, "[error]") && strings.Contains(t, ".") {
			unformatted = append(unformatted, t)
		}
	}
	if len(unformatted) > 0 {
		out := []string{fmt.Sprintf("%d files need formatting:", len(unformatted))}
		out = append(out, unformatted...)
		return out
	}
	if len(warnings) > 0 {
		return []string{fmt.Sprintf("%d warnings", len(warnings))}
	}
	if len(lines) <= 5 {
		return lines
	}
	return append(lines[:5], fmt.Sprintf("... (%d more lines)", len(lines)-5))
}

// ── ruby ─────────────────────────────────────────────────────────────────────

func compressRuby(cmd string, lines []string) []string {
	switch {
	case strings.Contains(cmd, "rubocop"):
		return compressRubocop(lines)
	case strings.Contains(cmd, "bundle install") || strings.Contains(cmd, "bundle update"):
		return compressBundle(lines)
	case strings.Contains(cmd, "rake test") || strings.Contains(cmd, "rails test"):
		return compressMinitest(lines)
	}
	return compactLines(lines, 15)
}

func compressRubocop(lines []string) []string {
	var offenses []string
	var filesInspected, totalOffenses int
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.Contains(t, "files inspected") {
			for _, word := range strings.Fields(t) {
				if n := parseInt(word); n > 0 {
					filesInspected = n
					break
				}
			}
			if idx := strings.Index(t, "offense"); idx >= 0 {
				before := t[:idx]
				parts := strings.Fields(before)
				if len(parts) > 0 {
					totalOffenses = parseInt(parts[len(parts)-1])
				}
			}
		} else if strings.Contains(t, ": C:") || strings.Contains(t, ": W:") ||
			strings.Contains(t, ": E:") || strings.Contains(t, ": F:") {
			offenses = append(offenses, t)
		}
	}
	if filesInspected == 0 && len(offenses) == 0 {
		return compactLines(lines, 15)
	}
	result := fmt.Sprintf("rubocop: %d files, %d offenses", filesInspected, totalOffenses)
	if totalOffenses == 0 {
		return []string{result + " (clean)"}
	}
	grouped := groupByCop(offenses)
	out := []string{result}
	for i, pair := range grouped {
		if i >= 10 {
			break
		}
		out = append(out, fmt.Sprintf("  %s: %dx", pair[0], parseInt(pair[1])))
	}
	if len(offenses) > 10 {
		out = append(out, fmt.Sprintf("  ... +%d more", len(offenses)-10))
	}
	return out
}

// groupByCop returns [cop, count] pairs sorted by count descending.
func groupByCop(offenses []string) [][2]string {
	m := make(map[string]int)
	for _, o := range offenses {
		cop := "unknown"
		if idx := strings.LastIndex(o, "["); idx >= 0 {
			if end := strings.Index(o[idx:], "]"); end >= 0 {
				cop = o[idx+1 : idx+end]
			}
		}
		m[cop]++
	}
	pairs := make([][2]string, 0, len(m))
	for k, v := range m {
		pairs = append(pairs, [2]string{k, fmt.Sprintf("%d", v)})
	}
	sort.Slice(pairs, func(i, j int) bool {
		return parseInt(pairs[i][1]) > parseInt(pairs[j][1])
	})
	return pairs
}

func compressBundle(lines []string) []string {
	var installed, using int
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "Installing ") {
			installed++
		} else if strings.HasPrefix(t, "Using ") {
			using++
		}
	}
	if installed == 0 && using == 0 {
		return compactLines(lines, 10)
	}
	result := "bundle: "
	var parts []string
	if installed > 0 {
		parts = append(parts, fmt.Sprintf("%d installed", installed))
	}
	if using > 0 {
		parts = append(parts, fmt.Sprintf("%d using (cached)", using))
	}
	result += strings.Join(parts, ", ")
	// append trailing "Bundle complete" line if present
	for i := len(lines) - 1; i >= 0 && i >= len(lines)-3; i-- {
		t := strings.TrimSpace(lines[i])
		if strings.HasPrefix(t, "Bundle complete") || strings.HasPrefix(t, "Bundled gems") {
			result += "\n  " + t
			break
		}
	}
	return strings.Split(result, "\n")
}

func compressMinitest(lines []string) []string {
	var total, failures, errors, skips int
	var timeStr string
	var failureDetails []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.Contains(t, "runs,") && strings.Contains(t, "assertions") {
			for _, part := range strings.Split(t, ", ") {
				part = strings.TrimSpace(part)
				switch {
				case strings.HasSuffix(part, "runs"):
					total = parseInt(strings.Fields(part)[0])
				case strings.HasSuffix(part, "failures"):
					failures = parseInt(strings.Fields(part)[0])
				case strings.HasSuffix(part, "errors"):
					errors = parseInt(strings.Fields(part)[0])
				case strings.HasSuffix(part, "skips"):
					skips = parseInt(strings.Fields(part)[0])
				}
			}
			if idx := strings.Index(t, " in "); idx >= 0 {
				timeStr = strings.SplitN(t[idx+4:], ",", 2)[0]
			}
		}
		if strings.HasPrefix(t, "Failure:") || strings.HasPrefix(t, "Error:") {
			failureDetails = append(failureDetails, t)
		}
	}
	if total == 0 {
		return compactLines(lines, 10)
	}
	passed := total - failures - errors - skips
	if passed < 0 {
		passed = 0
	}
	result := fmt.Sprintf("minitest: %d passed", passed)
	if failures > 0 {
		result += fmt.Sprintf(", %d failed", failures)
	}
	if errors > 0 {
		result += fmt.Sprintf(", %d errors", errors)
	}
	if skips > 0 {
		result += fmt.Sprintf(", %d skipped", skips)
	}
	if timeStr != "" {
		result += fmt.Sprintf(" (%s)", strings.TrimSpace(timeStr))
	}
	out := []string{result}
	for i, d := range failureDetails {
		if i >= 5 {
			break
		}
		out = append(out, "  "+d)
	}
	return out
}

// ── composer ──────────────────────────────────────────────────────────────────

func compressComposer(cmd string, lines []string) []string {
	joined := strings.Join(lines, "\n")
	if strings.TrimSpace(joined) == "" {
		return []string{"ok"}
	}
	if strings.Contains(cmd, "install") || strings.Contains(cmd, "update") || strings.Contains(cmd, "require") {
		return compressComposerInstall(lines)
	}
	if strings.Contains(cmd, "outdated") {
		return compressComposerOutdated(lines)
	}
	return compactLines(lines, 15)
}

func compressComposerInstall(lines []string) []string {
	var installed, updated, removed int
	loading := false
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "- Installing") || strings.HasPrefix(t, "- Downloading") {
			installed++
		} else if strings.HasPrefix(t, "- Updating") || strings.HasPrefix(t, "- Upgrading") {
			updated++
		} else if strings.HasPrefix(t, "- Removing") {
			removed++
		} else if strings.HasPrefix(t, "Loading composer") {
			loading = true
		}
	}
	if !loading && installed == 0 && updated == 0 {
		return compactLines(lines, 10)
	}
	var parts []string
	if installed > 0 {
		parts = append(parts, fmt.Sprintf("%d installed", installed))
	}
	if updated > 0 {
		parts = append(parts, fmt.Sprintf("%d updated", updated))
	}
	if removed > 0 {
		parts = append(parts, fmt.Sprintf("%d removed", removed))
	}
	result := "composer: " + strings.Join(parts, ", ")
	for i := len(lines) - 1; i >= 0; i-- {
		l := lines[i]
		if strings.Contains(l, "Package operations") || strings.Contains(l, "Nothing to install") {
			result += "\n  " + strings.TrimSpace(l)
			break
		}
	}
	return strings.Split(result, "\n")
}

func compressComposerOutdated(lines []string) []string {
	var kept []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t != "" && !strings.HasPrefix(t, "Color legend") {
			kept = append(kept, l)
		}
	}
	if len(kept) == 0 {
		return []string{"all up to date"}
	}
	if len(kept) <= 20 {
		return kept
	}
	out := []string{fmt.Sprintf("%d outdated packages:", len(kept))}
	out = append(out, kept[:15]...)
	out = append(out, fmt.Sprintf("... (%d more)", len(kept)-15))
	return out
}

// ── artisan ───────────────────────────────────────────────────────────────────

var (
	reArtisanMigStatus = regexp.MustCompile(`\|\s*(Ran|Pending)\s*\|\s*(.+?)\s*\|`)
	reArtisanRoute     = regexp.MustCompile(`(GET|POST|PUT|PATCH|DELETE|ANY)\s*\|\s*(\S+)\s*\|\s*(\S+)`)
	reArtisanTestResult = regexp.MustCompile(`Tests:\s*(\d+)\s*passed(?:,\s*(\d+)\s*failed)?`)
	reArtisanPest      = regexp.MustCompile(`(\d+)\s*passed.*?(\d+)\s*failed|(\d+)\s*passed`)
)

func compressArtisan(cmd string, lines []string) []string {
	joined := strings.Join(lines, "\n")
	if strings.TrimSpace(joined) == "" {
		return []string{"ok"}
	}
	switch {
	case strings.Contains(cmd, "migrate") && strings.Contains(cmd, "--status"):
		return compressArtisanMigrateStatus(lines)
	case strings.Contains(cmd, "migrate"):
		return compressArtisanMigrate(lines)
	case strings.Contains(cmd, "test"):
		return compressArtisanTest(lines)
	case strings.Contains(cmd, "route:list"):
		return compressArtisanRoutes(lines)
	case strings.Contains(cmd, "make:"):
		return compressArtisanMake(lines)
	case strings.Contains(cmd, "queue:work") || strings.Contains(cmd, "queue:listen"):
		return compressArtisanQueue(lines)
	}
	return compactLines(lines, 10)
}

func compressArtisanMigrate(lines []string) []string {
	var ran int
	var errors []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.Contains(t, "Migrating:") || strings.Contains(t, "DONE") {
			ran++
		}
		if strings.HasPrefix(t, "SQLSTATE") || strings.Contains(t, "ERROR") || strings.Contains(t, "Exception") {
			errors = append(errors, t)
		}
	}
	if len(errors) > 0 {
		return append([]string{"migrate FAILED:"}, errors...)
	}
	if ran > 0 {
		return []string{fmt.Sprintf("migrated %d tables", ran)}
	}
	for _, l := range lines {
		if strings.Contains(l, "Nothing to migrate") {
			return []string{"nothing to migrate"}
		}
	}
	return compactLines(lines, 5)
}

func compressArtisanMigrateStatus(lines []string) []string {
	joined := strings.Join(lines, "\n")
	matches := reArtisanMigStatus.FindAllStringSubmatch(joined, -1)
	if len(matches) == 0 {
		return compactLines(lines, 10)
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

func compressArtisanTest(lines []string) []string {
	var passed, failed int
	var failures []string
	var timeStr string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if m := reArtisanTestResult.FindStringSubmatch(t); m != nil {
			passed = parseInt(m[1])
			if m[2] != "" {
				failed = parseInt(m[2])
			}
		}
		if m := reArtisanPest.FindStringSubmatch(t); m != nil {
			if m[3] != "" {
				passed = parseInt(m[3])
			} else {
				passed = parseInt(m[1])
				failed = parseInt(m[2])
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

func compressArtisanRoutes(lines []string) []string {
	joined := strings.Join(lines, "\n")
	matches := reArtisanRoute.FindAllStringSubmatch(joined, -1)
	if len(matches) == 0 {
		return compactLines(lines, 15)
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

func compressArtisanMake(lines []string) []string {
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.Contains(t, "created successfully") || strings.Contains(t, ".php") {
			return []string{t}
		}
	}
	return []string{"created"}
}

func compressArtisanQueue(lines []string) []string {
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
		return compactLines(lines, 5)
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

// ── mix ────────────────────────────────────────────────────────────────────────

func compressMix(cmd string, lines []string) []string {
	joined := strings.Join(lines, "\n")
	if strings.TrimSpace(joined) == "" {
		return []string{"ok"}
	}
	switch {
	case strings.Contains(cmd, "test"):
		return compressMixTest(lines)
	case strings.Contains(cmd, "deps.get") || strings.Contains(cmd, "deps.compile"):
		return compressMixDeps(lines)
	case strings.Contains(cmd, "compile") || strings.Contains(cmd, "build"):
		return compressMixCompile(lines)
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
		return compressMixLint(lines)
	}
	return compactLines(lines, 15)
}

func compressMixTest(lines []string) []string {
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
	return compactLines(lines, 10)
}

func compressMixDeps(lines []string) []string {
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
		return compactLines(lines, 5)
	}
	return []string{fmt.Sprintf("deps: %d resolved, %d compiled", resolved, compiled)}
}

func compressMixCompile(lines []string) []string {
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

func compressMixLint(lines []string) []string {
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
		return compactLines(lines, 10)
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

// ── swift build ───────────────────────────────────────────────────────────────

func compressSwiftBuild(cmd string, lines []string) []string {
	joined := strings.Join(lines, "\n")
	if strings.TrimSpace(joined) == "" {
		return []string{"ok"}
	}
	switch {
	case strings.Contains(cmd, "test"):
		return compressSwiftTest(lines)
	case strings.Contains(cmd, "build"):
		return compressSwiftBuildOutput(lines)
	case strings.Contains(cmd, "package resolve") || strings.Contains(cmd, "package update"):
		return compressSwiftResolve(lines)
	}
	return compactLines(lines, 15)
}

func compressSwiftTest(lines []string) []string {
	var passed, failed int
	var failures []string
	var timeStr string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.Contains(t, "Test Case") && strings.Contains(t, "passed") {
			passed++
		} else if strings.Contains(t, "Test Case") && strings.Contains(t, "failed") {
			failed++
			failures = append(failures, t)
		}
		if strings.HasPrefix(t, "Test Suite") && strings.Contains(t, "Executed") {
			timeStr = t
		}
		if strings.Contains(t, "Executed") && strings.Contains(t, "tests") && timeStr == "" {
			if idx := strings.Index(t, "Executed"); idx >= 0 {
				timeStr = t[idx:]
			}
		}
	}
	if passed == 0 && failed == 0 {
		return compactLines(lines, 10)
	}
	result := fmt.Sprintf("swift test: %d passed", passed)
	if failed > 0 {
		result += fmt.Sprintf(", %d failed", failed)
	}
	out := []string{result}
	if timeStr != "" {
		out = append(out, "  "+timeStr)
	}
	for i, f := range failures {
		if i >= 5 {
			break
		}
		out = append(out, "  FAIL: "+f)
	}
	return out
}

func compressSwiftBuildOutput(lines []string) []string {
	var compiling, warnings int
	linking := false
	var errors []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "Compiling") || (strings.Contains(t, "[") && strings.Contains(t, "]")) {
			compiling++
		}
		if strings.HasPrefix(t, "Linking") || strings.Contains(t, "Linking") {
			linking = true
		}
		if strings.Contains(t, "error:") {
			errors = append(errors, t)
		}
		if strings.Contains(t, "warning:") {
			warnings++
		}
	}
	if len(errors) > 0 {
		result := fmt.Sprintf("%d errors", len(errors))
		if warnings > 0 {
			result += fmt.Sprintf(", %d warnings", warnings)
		}
		out := []string{result}
		for i, e := range errors {
			if i >= 10 {
				break
			}
			out = append(out, "  "+e)
		}
		return out
	}
	result := fmt.Sprintf("Build ok (%d compiled", compiling)
	if linking {
		result += ", linked"
	}
	if warnings > 0 {
		result += fmt.Sprintf(", %d warnings", warnings)
	}
	result += ")"
	return []string{result}
}

func compressSwiftResolve(lines []string) []string {
	var fetched, resolved int
	for _, l := range lines {
		if strings.Contains(l, "Fetching") {
			fetched++
		}
		if strings.Contains(l, "Resolving") || strings.Contains(l, "resolved") {
			resolved++
		}
	}
	if fetched == 0 && resolved == 0 {
		return compactLines(lines, 5)
	}
	return []string{fmt.Sprintf("%d fetched, %d resolved", fetched, resolved)}
}

// ── zig ───────────────────────────────────────────────────────────────────────

func compressZig(cmd string, lines []string) []string {
	joined := strings.Join(lines, "\n")
	if strings.TrimSpace(joined) == "" {
		return []string{"ok"}
	}
	if strings.Contains(cmd, "test") {
		return compressZigTest(lines)
	}
	if strings.Contains(cmd, "build") {
		return compressZigBuild(lines)
	}
	return compactLines(lines, 15)
}

func compressZigTest(lines []string) []string {
	var passed, failed int
	var failures []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.Contains(t, "1/1 test") || strings.Contains(t, "test passed") {
			passed++
		}
		if strings.Contains(t, "FAIL") || strings.Contains(t, "test failed") {
			failed++
			failures = append(failures, t)
		}
		if strings.Contains(t, "All") && strings.Contains(t, "passed") {
			parts := strings.Fields(t)
			if len(parts) >= 2 {
				if n := parseInt(parts[1]); n > 0 {
					passed = n
				}
			}
		}
	}
	if passed == 0 && failed == 0 {
		return compactLines(lines, 10)
	}
	result := fmt.Sprintf("zig test: %d passed", passed)
	if failed > 0 {
		result += fmt.Sprintf(", %d failed", failed)
	}
	out := []string{result}
	for i, f := range failures {
		if i >= 5 {
			break
		}
		out = append(out, "  "+f)
	}
	return out
}

func compressZigBuild(lines []string) []string {
	var errors, warnings []string
	for _, l := range lines {
		if strings.Contains(l, "error:") || strings.Contains(l, "Error") {
			errors = append(errors, strings.TrimSpace(l))
		}
		if strings.Contains(l, "warning:") {
			warnings = append(warnings, strings.TrimSpace(l))
		}
	}
	if len(errors) > 0 {
		result := fmt.Sprintf("%d errors", len(errors))
		if len(warnings) > 0 {
			result += fmt.Sprintf(", %d warnings", len(warnings))
		}
		out := []string{result}
		for i, e := range errors {
			if i >= 10 {
				break
			}
			out = append(out, "  "+e)
		}
		return out
	}
	if len(warnings) > 0 {
		return []string{fmt.Sprintf("ok (%d warnings)", len(warnings))}
	}
	return []string{"ok"}
}

// ── ps / du / ping ────────────────────────────────────────────────────────────

func compressPs(lines []string) []string {
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
			cpu := parseFloat(cols[2])
			mem := parseFloat(cols[3])
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

func compressDu(lines []string) []string {
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
			entries = append(entries, entry{parseSizeField(strings.TrimSpace(parts[0])), strings.TrimSpace(parts[1])})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].size > entries[j].size })
	out := []string{fmt.Sprintf("du: %d entries (top 15 by size)", len(nonEmpty))}
	for i, e := range entries {
		if i >= 15 {
			break
		}
		out = append(out, fmt.Sprintf("%s\t%s", formatDuSize(e.size), e.path))
	}
	return out
}

func compressPing(lines []string) []string {
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

func compressSystemd(cmd string, lines []string) []string {
	joined := strings.Join(lines, "\n")
	if strings.TrimSpace(joined) == "" {
		return []string{"ok"}
	}
	if strings.HasPrefix(cmd, "systemctl") {
		return compressSystemctl(cmd, lines)
	}
	if strings.HasPrefix(cmd, "journalctl") {
		return compressJournal(lines)
	}
	return compactLines(lines, 15)
}

func compressSystemctl(cmd string, lines []string) []string {
	if strings.Contains(cmd, "status") {
		return compressSystemctlStatus(lines)
	}
	if strings.Contains(cmd, "list-units") || strings.Contains(cmd, "list-unit-files") ||
		(!strings.Contains(cmd, "start") && !strings.Contains(cmd, "stop") &&
			!strings.Contains(cmd, "restart") && !strings.Contains(cmd, "enable") &&
			!strings.Contains(cmd, "disable")) {
		return compressSystemctlList(lines)
	}
	return compactLines(lines, 10)
}

func compressSystemctlStatus(lines []string) []string {
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
		return compactLines(lines, 10)
	}
	return parts
}

func compressSystemctlList(lines []string) []string {
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

func compressJournal(lines []string) []string {
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

func compressLs(lines []string) []string {
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
		return compressLsLong(lines)
	}
	return compressLsShort(lines)
}

func compressLsLong(lines []string) []string {
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
			size := lsFormatSize(parts[4])
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

func compressLsShort(lines []string) []string {
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

func lsFormatSize(sizeStr string) string {
	if len(sizeStr) == 0 {
		return "0B"
	}
	last := sizeStr[len(sizeStr)-1]
	if last == 'K' || last == 'M' || last == 'G' || last == 'T' {
		return sizeStr
	}
	n := parseUint64(sizeStr)
	switch {
	case n >= 1_048_576:
		return fmt.Sprintf("%.1fM", float64(n)/1_048_576)
	case n >= 1024:
		return fmt.Sprintf("%.1fK", float64(n)/1024)
	default:
		return fmt.Sprintf("%dB", n)
	}
}

// ── mysql / psql ──────────────────────────────────────────────────────────────

func compressMySQL(cmd string, lines []string) []string {
	joined := strings.TrimSpace(strings.Join(lines, "\n"))
	if joined == "" {
		return []string{"ok"}
	}
	if isMySQLTableOutput(lines) {
		return compressMySQLTable(lines)
	}
	if strings.Contains(cmd, "show databases") || strings.Contains(cmd, "show tables") {
		return compressMySQLShow(lines)
	}
	if strings.HasPrefix(joined, "Query OK") || strings.HasPrefix(joined, "Empty set") {
		return []string{strings.Split(joined, "\n")[0]}
	}
	return compactLines(lines, 20)
}

func isMySQLTableOutput(lines []string) bool {
	for _, l := range lines {
		if strings.HasPrefix(l, "+") && strings.Contains(l, "---") {
			return true
		}
	}
	return false
}

func compressMySQLTable(lines []string) []string {
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
	out := lines[:previewEnd]
	return append(out, fmt.Sprintf("... (%d rows total)", rowCount))
}

func compressMySQLShow(lines []string) []string {
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

func compressPsql(cmd string, lines []string) []string {
	joined := strings.TrimSpace(strings.Join(lines, "\n"))
	if joined == "" {
		return []string{"ok"}
	}
	if isPsqlTableOutput(lines) {
		return compressPsqlTable(lines)
	}
	if strings.Contains(cmd, `\dt`) || strings.Contains(cmd, `\d`) {
		return compressPsqlDescribe(lines)
	}
	for _, prefix := range []string{"INSERT", "UPDATE", "DELETE", "CREATE", "ALTER", "DROP"} {
		if strings.HasPrefix(joined, prefix) {
			return []string{strings.Split(joined, "\n")[0]}
		}
	}
	return compactLines(lines, 20)
}

func isPsqlTableOutput(lines []string) bool {
	for _, l := range lines {
		if strings.Contains(l, "---+---") || strings.Contains(l, "-+-") {
			return true
		}
	}
	return false
}

func compressPsqlTable(lines []string) []string {
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
	out := lines[:previewEnd]
	return append(out, "... "+countStr)
}

func compressPsqlDescribe(lines []string) []string {
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

// ── env filter ────────────────────────────────────────────────────────────────

var envSensitivePatterns = []string{
	"KEY", "SECRET", "TOKEN", "PASSWORD", "PASSWD", "CREDENTIALS",
	"AUTH", "API_KEY", "PRIVATE", "CERT",
}

func compressEnvFilter(lines []string) []string {
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
		if idx := strings.Index(t, "="); idx > 0 {
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
			displayVal := value
			if isSensitive {
				displayVal = "***"
			} else if len(value) > 80 {
				displayVal = value[:40] + "..."
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

// ── helpers ───────────────────────────────────────────────────────────────────

// compactLines returns at most max non-empty lines from the input slice.
// It reuses the global compactLines in server_compress.go via the different
// signature: this one takes []string directly.
func compactLinesFiltered(lines []string, max int) []string {
	var nonEmpty []string
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			nonEmpty = append(nonEmpty, l)
		}
	}
	if len(nonEmpty) <= max {
		return nonEmpty
	}
	return append(nonEmpty[:max], fmt.Sprintf("... (%d more lines)", len(nonEmpty)-max))
}

func parseInt(s string) int {
	s = strings.TrimSpace(s)
	n := 0
	for _, c := range s {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		} else {
			break
		}
	}
	return n
}

func parseFloat(s string) float64 {
	var n float64
	fmt.Sscanf(strings.TrimSpace(s), "%f", &n)
	return n
}

func parseUint64(s string) uint64 {
	var n uint64
	for _, c := range strings.TrimSpace(s) {
		if c >= '0' && c <= '9' {
			n = n*10 + uint64(c-'0')
		} else {
			break
		}
	}
	return n
}

func parseSizeField(s string) uint64 {
	s = strings.TrimSpace(s)
	if n, err := fmt.Sscanf(s, "%d", new(uint64)); n == 1 && err == nil {
		var v uint64
		fmt.Sscanf(s, "%d", &v)
		return v
	}
	if len(s) == 0 {
		return 0
	}
	last := s[len(s)-1]
	prefix := s[:len(s)-1]
	var base float64
	fmt.Sscanf(prefix, "%f", &base)
	switch last {
	case 'K', 'k':
		return uint64(base * 1024)
	case 'M', 'm':
		return uint64(base * 1024 * 1024)
	case 'G', 'g':
		return uint64(base * 1024 * 1024 * 1024)
	}
	var v uint64
	fmt.Sscanf(s, "%d", &v)
	return v
}

func formatDuSize(bytes uint64) string {
	switch {
	case bytes >= 1_073_741_824:
		return fmt.Sprintf("%.1fG", float64(bytes)/1_073_741_824)
	case bytes >= 1_048_576:
		return fmt.Sprintf("%.1fM", float64(bytes)/1_048_576)
	case bytes >= 1024:
		return fmt.Sprintf("%.0fK", float64(bytes)/1024)
	default:
		return fmt.Sprintf("%d", bytes)
	}
}
