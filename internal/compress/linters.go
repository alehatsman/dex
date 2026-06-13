package compress

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ── eslint ────────────────────────────────────────────────────────────────────

var (
	reEslintFile = &lazyRe{pattern: `^(/[^\s]+\.(ts|js|tsx|jsx|vue|svelte|mjs|cjs)|[./][^\s]+\.(ts|js|tsx|jsx|vue|svelte|mjs|cjs))$`}
	reEslintDiag = &lazyRe{pattern: `^\s+(\d+):(\d+)\s+(error|warning)\s+(.+?)\s{2,}(\S+)\s*$`}
)

func CompressEslint(lines []string) []string {
	type diagEntry struct {
		rule string
		msg  string
		loc  string
	}
	var errors, warnings []diagEntry
	var summaryLine string

	for _, l := range lines {
		if reEslintFile.MatchString(strings.TrimSpace(l)) {
			continue // skip file path lines
		}
		if m := reEslintDiag.FindStringSubmatch(l); m != nil {
			d := diagEntry{rule: m[5], msg: m[4], loc: m[1] + ":" + m[2]}
			if m[3] == "error" {
				errors = append(errors, d)
			} else {
				warnings = append(warnings, d)
			}
			continue
		}
		t := strings.TrimSpace(l)
		if strings.Contains(t, "problem") || strings.Contains(t, "error") || strings.Contains(t, "warning") {
			if strings.Contains(t, "✖") || strings.Contains(t, "✗") || strings.Contains(t, "×") ||
				strings.Contains(t, "problems") {
				summaryLine = t
			}
		}
	}

	if len(errors) == 0 && len(warnings) == 0 {
		return lines
	}

	var out []string
	if summaryLine != "" {
		out = append(out, summaryLine)
	} else {
		out = append(out, fmt.Sprintf("%d errors, %d warnings", len(errors), len(warnings)))
	}

	// Group by rule, show top rules.
	ruleCount := make(map[string]int)
	for _, e := range errors {
		ruleCount[e.rule]++
	}
	for _, e := range warnings {
		ruleCount[e.rule]++
	}
	type rulePair struct {
		rule  string
		count int
	}
	var rules []rulePair
	for r, c := range ruleCount {
		rules = append(rules, rulePair{r, c})
	}
	sort.Slice(rules, func(i, j int) bool { return rules[i].count > rules[j].count })
	for i, r := range rules {
		if i >= 5 {
			break
		}
		out = append(out, fmt.Sprintf("  %s (%d)", r.rule, r.count))
	}
	return out
}

// ── ruff ─────────────────────────────────────────────────────────────────────

var (
	reRuffIssue    = &lazyRe{pattern: `^([^:]+):(\d+):(\d+): ([A-Z]\d+) (.+)$`}
	reRuffSummary  = &lazyRe{pattern: `Found \d+ error`}
	reRuffFixed    = &lazyRe{pattern: `Fixed \d+`}
	reRuffNoIssues = &lazyRe{pattern: `All checks passed`}
)

func CompressRuff(cmd string, lines []string) []string {
	_ = cmd
	for _, l := range lines {
		if reRuffNoIssues.MatchString(l) {
			return []string{"clean"}
		}
	}
	type issueEntry struct {
		file string
		code string
		msg  string
	}
	var issues []issueEntry
	var summaryLine string
	for _, l := range lines {
		if m := reRuffIssue.FindStringSubmatch(l); m != nil {
			issues = append(issues, issueEntry{file: m[1], code: m[4], msg: m[5]})
		}
		if reRuffSummary.MatchString(l) || reRuffFixed.MatchString(l) {
			summaryLine = strings.TrimSpace(l)
		}
	}
	if len(issues) == 0 {
		return lines
	}
	header := fmt.Sprintf("%d issues", len(issues))
	if summaryLine != "" {
		header = summaryLine
	}
	out := []string{header}
	// Group by rule code.
	codeCount := make(map[string]int)
	codeExamples := make(map[string]string)
	for _, issue := range issues {
		codeCount[issue.code]++
		if _, ok := codeExamples[issue.code]; !ok {
			codeExamples[issue.code] = issue.file + ": " + issue.msg
		}
	}
	type codePair struct {
		code  string
		count int
	}
	var codes []codePair
	for c, n := range codeCount {
		codes = append(codes, codePair{c, n})
	}
	sort.Slice(codes, func(i, j int) bool { return codes[i].count > codes[j].count })
	for i, cp := range codes {
		if i >= 8 {
			break
		}
		out = append(out, fmt.Sprintf("  %s (%d): %s", cp.code, cp.count, codeExamples[cp.code]))
	}
	return out
}

// ── mypy ──────────────────────────────────────────────────────────────────────

var (
	reMypy        = &lazyRe{pattern: `^([^:]+):(\d+): (error|note|warning): (.+?) \[([^\]]+)\]$`}
	reMypySummary = &lazyRe{pattern: `^Found \d+ error`}
	reMypySuccess = &lazyRe{pattern: `^Success: no issues`}
)

func CompressMypy(lines []string) []string {
	for _, l := range lines {
		if reMypySuccess.MatchString(l) {
			return []string{"clean"}
		}
	}
	type mypyIssue struct {
		file string
		code string
		msg  string
	}
	var errors []mypyIssue
	var summaryLine string
	for _, l := range lines {
		if m := reMypy.FindStringSubmatch(l); m != nil && m[3] == "error" {
			errors = append(errors, mypyIssue{file: m[1], code: m[5], msg: m[4]})
		}
		if reMypySummary.MatchString(l) {
			summaryLine = strings.TrimSpace(l)
		}
	}
	if len(errors) == 0 && summaryLine == "" {
		return lines
	}
	out := []string{summaryLine}
	for i, e := range errors {
		if i >= 10 {
			out = append(out, fmt.Sprintf("  … +%d more", len(errors)-10))
			break
		}
		out = append(out, fmt.Sprintf("  %s [%s]: %s", e.file, e.code, e.msg))
	}
	return out
}

// ── tsc ───────────────────────────────────────────────────────────────────────

var (
	reTsc      = &lazyRe{pattern: `^([^(]+)\((\d+),(\d+)\): (error|warning) (TS\d+): (.+)$`}
	reTscFound = &lazyRe{pattern: `Found \d+ error`}
)

func CompressTsc(lines []string) []string {
	type tscError struct {
		file string
		code string
		msg  string
	}
	var errors []tscError
	var summaryLine string
	for _, l := range lines {
		if m := reTsc.FindStringSubmatch(l); m != nil {
			errors = append(errors, tscError{file: strings.TrimSpace(m[1]), code: m[5], msg: m[6]})
		}
		if reTscFound.MatchString(l) {
			summaryLine = strings.TrimSpace(l)
		}
	}
	if len(errors) == 0 {
		return lines
	}
	// Count files.
	fileSet := make(map[string]struct{})
	for _, e := range errors {
		fileSet[e.file] = struct{}{}
	}
	header := fmt.Sprintf("%d errors in %d files", len(errors), len(fileSet))
	if summaryLine != "" {
		header = summaryLine + fmt.Sprintf(" (in %d files)", len(fileSet))
	}
	out := []string{header}
	for i, e := range errors {
		if i >= 10 {
			out = append(out, fmt.Sprintf("  … +%d more", len(errors)-10))
			break
		}
		out = append(out, fmt.Sprintf("  %s %s: %s", e.file, e.code, e.msg))
	}
	return out
}

// ── prisma ────────────────────────────────────────────────────────────────────

var (
	rePrismaBlockChars = regexp.MustCompile(`[▸▹►▻▶►]`)
)

func CompressPrisma(cmd string, lines []string) []string {
	switch {
	case strings.Contains(cmd, "generate"):
		return CompressPrismaGenerate(lines)
	case strings.Contains(cmd, "migrate"):
		return CompressPrismaMigrate(lines)
	case strings.Contains(cmd, "db push") || strings.Contains(cmd, "db pull"):
		return CompressPrismaDBSync(lines)
	}
	return CompressPrismaStripNoise(lines)
}

func CompressPrismaGenerate(lines []string) []string {
	var out []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if rePrismaBlockChars.MatchString(t) || strings.Contains(t, "Generating") ||
			strings.Contains(t, "Generated Prisma Client") || strings.Contains(t, "Start by importing") ||
			strings.Contains(t, "import { PrismaClient }") {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return CompactLines(lines, 10)
	}
	return out
}

func CompressPrismaMigrate(lines []string) []string {
	var out []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.Contains(t, "migration") || strings.Contains(t, "Migration") ||
			strings.Contains(t, "applied") || strings.Contains(t, "created") ||
			strings.Contains(t, "Your database is now in sync") || strings.HasPrefix(t, "Error") {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return CompactLines(lines, 10)
	}
	return out
}

func CompressPrismaDBSync(lines []string) []string {
	var out []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.Contains(t, "in sync") || strings.Contains(t, "changes") ||
			strings.Contains(t, "created") || strings.HasPrefix(t, "Error") {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return CompactLines(lines, 10)
	}
	return out
}

func CompressPrismaStripNoise(lines []string) []string {
	var out []string
	for _, l := range lines {
		if !rePrismaBlockChars.MatchString(l) {
			out = append(out, l)
		}
	}
	if len(out) == 0 {
		return lines
	}
	return out
}

// ── prettier ──────────────────────────────────────────────────────────────────

func CompressPrettier(lines []string) []string {
	var written, unchanged, errors int
	var errorLines []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasSuffix(t, "(unchanged)") {
			unchanged++
		} else if strings.HasSuffix(t, "(written)") || strings.Contains(t, "ms") {
			written++
		} else if strings.HasPrefix(t, "[error]") || strings.HasPrefix(t, "Error") {
			errors++
			errorLines = append(errorLines, t)
		}
	}
	if written == 0 && unchanged == 0 && errors == 0 {
		return lines
	}
	result := fmt.Sprintf("prettier: %d written, %d unchanged", written, unchanged)
	if errors > 0 {
		result += fmt.Sprintf(", %d errors", errors)
	}
	out := []string{result}
	for i, e := range errorLines {
		if i >= 5 {
			break
		}
		out = append(out, "  "+e)
	}
	return out
}
