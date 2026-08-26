// Package summarize holds the composition core of the view_summarize / read
// capability: pure line-slicing and the index-sourced signature, map, system
// prompt, and task-symbol formatting helpers. It is transport-agnostic — no
// sdk types, no *Server, no file IO. Orchestration (mode dispatch, caching,
// chat calls, sdk result building) stays in internal/mcp.
package summarize

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/alehatsman/dex/internal/chunk"
	"github.com/alehatsman/dex/internal/store"
)

// ParseLinesRange parses a lines:* range spec into a 1-indexed start/end. A
// returned 0 means "open" — start of file or end of file — which SliceLines
// clamps to the actual extents. Four forms are accepted:
//
//	N-M  lines N through M (inclusive)  → (N, M)
//	N-   line N through end of file     → (N, 0)
//	-M   first line through M           → (0, M)
//	N    the single line N              → (N, N)
//
// Negatives, a bare "-", a zero index, multi-dash specs, and an end before the
// start are rejected.
func ParseLinesRange(s string) (start, end int, ok bool) {
	i := strings.IndexByte(s, '-')
	if i < 0 {
		// Single line "N".
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			return 0, 0, false
		}
		return n, n, true
	}
	left, right := s[:i], s[i+1:]
	if left == "" && right == "" {
		return 0, 0, false // bare "-"
	}
	if left != "" {
		n, err := strconv.Atoi(left)
		if err != nil || n < 1 {
			return 0, 0, false
		}
		start = n
	}
	if right != "" {
		// Atoi rejects a second dash ("10-15" from "5-10-15") and negatives.
		m, err := strconv.Atoi(right)
		if err != nil || m < 1 {
			return 0, 0, false
		}
		end = m
	}
	if start > 0 && end > 0 && end < start {
		return 0, 0, false
	}
	return start, end, true
}

// SignaturesView builds the mode=signatures view: a compact symbol index,
// followed by a call-graph related-files hint and any task-relevant symbol
// body. This is the whole recipe for the view — callers supply the file bytes
// and indexed symbols (and a store for the graph/session lookups) and get the
// composed text back.
func SignaturesView(ctx context.Context, st *store.Store, data []byte, syms []store.GraphSymbol, relPath string) string {
	return SignaturesViewFiltered(ctx, st, data, syms, relPath, nil)
}

// SignaturesViewFiltered is SignaturesView narrowed to a specific symbol set —
// `only`, a set of qualified names. Empty/nil means show every symbol in the
// file (the plain SignaturesView behavior). Used by the pipe `callees|`/
// `callers|signatures` terminal, which otherwise dumped every export of a
// resolved callee's containing file instead of just the resolved symbols
// (#232).
func SignaturesViewFiltered(ctx context.Context, st *store.Store, data []byte, syms []store.GraphSymbol, relPath string, only []string) string {
	content := formatSignatures(data, syms, relPath, only)
	if related := graphRelatedHint(ctx, st, relPath); related != "" {
		content += related
	}
	return inlineTaskSymbol(ctx, st, data, syms, content)
}

// MapView builds the mode=map view: imports + exported symbols, followed by a
// related-files hint and (when symbols exist) any task-relevant symbol body.
func MapView(ctx context.Context, st *store.Store, data []byte, syms []store.GraphSymbol, imports []string, relPath string) string {
	content := formatMap(relPath, syms, imports, data)
	if related := graphRelatedHint(ctx, st, relPath); related != "" {
		content += related
	}
	if len(syms) > 0 {
		content = inlineTaskSymbol(ctx, st, data, syms, content)
	}
	return content
}

// formatSignatures produces a compact symbol index for a file.
// Each exported symbol gets its declaration line; unexported symbols are
// listed without source. Output is ~10× smaller than mode=full.
//
// only, when non-empty, narrows the listing to just those qualified names —
// used when the caller already resolved a specific symbol set (e.g. a pipe's
// `callees | signatures` terminal) and wants those, not every export the
// containing file happens to have (#232).
func formatSignatures(src []byte, syms []store.GraphSymbol, relPath string, only []string) string {
	if len(only) > 0 {
		want := make(map[string]bool, len(only))
		for _, q := range only {
			want[q] = true
		}
		filtered := syms[:0:0]
		for _, sym := range syms {
			if want[sym.QualifiedName] {
				filtered = append(filtered, sym)
			}
		}
		syms = filtered
	}
	srcLines := bytes.Split(bytes.TrimRight(src, "\n"), []byte("\n"))
	totalLines := bytes.Count(src, []byte("\n")) + 1
	var b strings.Builder
	if len(only) > 0 {
		fmt.Fprintf(&b, "%s %dL (%d of file's symbols — resolved via callees/callers)\n\n", relPath, totalLines, len(syms))
	} else {
		fmt.Fprintf(&b, "%s %dL (%d symbols)\n\n", relPath, totalLines, len(syms))
	}

	isTypeKind := func(kind string) bool {
		return kind == "struct" || kind == "interface" || kind == "type"
	}
	// Only top-level named symbols (func/type/var/const) count as exported,
	// not struct fields, imports, or file-level nodes.
	// Go uses uppercase-first for export visibility; other languages expose all
	// named top-level symbols as part of the public API.
	isGoFile := strings.HasSuffix(relPath, ".go")
	exported := func(sym store.GraphSymbol) bool {
		if isScaffoldKind(sym.Kind) {
			return false
		}
		if !isGoFile {
			return len(sym.Name) > 0
		}
		return len(sym.Name) > 0 && sym.Name[0] >= 'A' && sym.Name[0] <= 'Z'
	}
	writeSym := func(sym store.GraphSymbol) {
		si := sym.StartLine - 1
		exp := exported(sym)
		if exp {
			marker := "⊛"
			fmt.Fprintf(&b, "%s %s (lines %d-%d)\n", marker, sym.QualifiedName, sym.StartLine, sym.EndLine)
			if si >= 0 && si < len(srcLines) {
				b.Write(srcLines[si])
				b.WriteByte('\n')
			}
		} else {
			fmt.Fprintf(&b, "  %s %s (lines %d-%d)\n", sym.Kind, sym.QualifiedName, sym.StartLine, sym.EndLine)
		}
	}
	for _, sym := range syms {
		if isTypeKind(sym.Kind) {
			writeSym(sym)
		}
	}
	for _, sym := range syms {
		if !isTypeKind(sym.Kind) {
			writeSym(sym)
		}
	}
	return b.String()
}

// isScaffoldKind reports whether sym.Kind is a synthetic/structural node —
// the whole-file scaffold, an import statement, or a struct field — none of
// which belong in a file's public-API listing. Shared by formatSignatures and
// formatMap so the excluded-kind set can't drift between the two views.
func isScaffoldKind(kind string) bool {
	return kind == "file" || kind == "import" || kind == "field"
}

// formatMap produces a compact dependency map for a file: its package-level
// imports and exported declarations, sourced from the index (no LLM, no file
// read). Unexported symbols are omitted so the output mirrors the public API.
func formatMap(relPath string, syms []store.GraphSymbol, imports []string, data []byte) string {
	var b strings.Builder
	fmt.Fprintf(&b, "FILE: %s\n\n", relPath)
	if len(imports) > 0 {
		b.WriteString("IMPORTS:\n")
		for _, imp := range imports {
			fmt.Fprintf(&b, "  %s\n", imp)
		}
		b.WriteByte('\n')
	}
	// Go uses uppercase-first for export visibility; other languages expose all
	// named top-level symbols.
	isGoFile := strings.HasSuffix(relPath, ".go")
	var exportedLines strings.Builder
	count := 0
	for _, sym := range syms {
		// The synthetic whole-file scaffold node and import/field nodes aren't
		// part of a file's public API — skip them like formatSignatures' own
		// exported() does, or a pure re-export barrel (whose only symbol is
		// that scaffold) reports one bogus "export" instead of its real
		// content (#232).
		if isScaffoldKind(sym.Kind) {
			continue
		}
		if len(sym.Name) == 0 {
			continue
		}
		if isGoFile && (sym.Name[0] < 'A' || sym.Name[0] > 'Z') {
			continue
		}
		fmt.Fprintf(&exportedLines, "  %s %s (lines %d-%d)\n", sym.Kind, sym.QualifiedName, sym.StartLine, sym.EndLine)
		count++
	}
	if count > 0 {
		fmt.Fprintf(&b, "EXPORTS (%d):\n", count)
		b.WriteString(exportedLines.String())
		return b.String()
	}
	// A pure re-export barrel (`export * from './x'`, no local declarations)
	// has no symbols of its own — list the re-export targets instead of an
	// empty EXPORTS section, so `map` stays useful for the barrel-heavy
	// pattern common in JS/TS workspaces (#232).
	if reexports := reExportLines(data); len(reexports) > 0 {
		fmt.Fprintf(&b, "RE-EXPORTS (%d):\n", len(reexports))
		for _, ln := range reexports {
			fmt.Fprintf(&b, "  %s\n", ln)
		}
	}
	return b.String()
}

// reExportMatch finds JS/TS barrel re-export statements: `export * from '...'`
// and `export * as NS from '...'`.
var reExportMatch = regexp.MustCompile(`export\s*\*\s*(as\s+\w+\s+)?from\s*['"]([^'"]+)['"]`)

// reExportLines scans raw file bytes for barrel re-export statements a graph
// extractor doesn't persist as queryable symbols (they're resolved in-memory
// at graph-build time for cross-package call resolution, not stored).
func reExportLines(data []byte) []string {
	var out []string
	for _, m := range reExportMatch.FindAllSubmatch(data, -1) {
		if len(m[1]) > 0 {
			out = append(out, fmt.Sprintf("re-export: * %sfrom '%s'", string(m[1]), string(m[2])))
		} else {
			out = append(out, fmt.Sprintf("re-export: * from '%s'", string(m[2])))
		}
	}
	return out
}

// SliceLines returns the byte slice of `data` between lines start and
// end (both 1-indexed, inclusive). Zero values mean "from start of
// file" / "to end of file". Returned start/end are clamped to the
// actual file extents so the caller can echo back what was used.
func SliceLines(data []byte, start, end int) ([]byte, int, int) {
	if start <= 0 && end <= 0 {
		return data, 1, chunk.LineCount(data)
	}
	if start <= 0 {
		start = 1
	}
	// Walk newlines once. Cheap and avoids splitting the whole file.
	var (
		startByte = -1
		endByte   = len(data)
		line      = 1
	)
	if start == 1 {
		startByte = 0
	}
	for i := range data {
		if data[i] != '\n' {
			continue
		}
		line++
		if startByte < 0 && line == start {
			startByte = i + 1
		}
		if end > 0 && line > end {
			endByte = i + 1
			break
		}
	}
	if startByte < 0 {
		// `start` is past EOF — return empty slice but record extents.
		return nil, start, start - 1
	}
	if end <= 0 || end > line {
		// Open end (to EOF) or past EOF: report the true last line. The walk's
		// `line` counts a phantom empty line after a trailing newline, so use
		// chunk.LineCount for consistency with the whole-file path (#672).
		end = chunk.LineCount(data)
	}
	return data[startByte:endByte], start, end
}

// BuildSystem builds the file-summarizer system prompt, optionally narrowed by
// a free-text focus.
func BuildSystem(focus string) string {
	base := "You are a file summarizer. Given a single file (or slice), produce a tight, factual summary the reader can use as a substitute for opening the file. " +
		"Lead with one sentence on what the file is for. Then a short bulleted list of the central items the file defines or exposes — picking the framing that fits the file kind: " +
		"exported types/functions for source code, targets and variables for Makefiles, top-level keys for config (YAML/TOML/JSON), section headings for docs, etc. " +
		"Also note key invariants, side effects, or constraints, and any non-obvious dependencies or cross-references. " +
		"Quote identifiers and names verbatim. No prose padding, no apologies, no restating the prompt. " +
		"Keep under 200 words. For trivial files (license, .gitignore, simple stubs) a single sentence is fine."
	if strings.TrimSpace(focus) != "" {
		base += " Focus specifically on: " + strings.TrimSpace(focus) + "."
	}
	return base
}

// BuildRollupSystem builds the directory-rollup system prompt. Unlike
// BuildSystem, the input is not source — it is the already-written summaries of
// the directory's members (child files and child subdirectories). The model's
// job is to synthesize a package/subsystem-level view from them, not to re-read
// code. Optionally narrowed by a free-text focus.
func BuildRollupSystem(focus string) string {
	base := "You are a codebase rollup summarizer. You are given the summaries of the members of one directory (a package or subsystem) — its child files and child subdirectories, each labeled by name. " +
		"Synthesize a single directory-level summary the reader can use to understand what this part of the codebase does without opening its members. " +
		"Lead with one sentence on the directory's overall responsibility. Then a short bulleted list of the main capabilities or components it groups, attributing each to the member(s) that provide it by name. " +
		"Note cross-member relationships, the directory's role relative to its siblings, and any load-bearing invariants that span members. " +
		"Quote package, file, and identifier names verbatim. Do not invent members not present in the input. No prose padding, no apologies, no restating the prompt. " +
		"Keep under 200 words."
	if strings.TrimSpace(focus) != "" {
		base += " Focus specifically on: " + strings.TrimSpace(focus) + "."
	}
	return base
}

// graphRelatedHint returns a compact "Related (call graph): ..." line
// listing files graph-adjacent to relPath, or "" when the graph is absent
// or has no neighbors. Never fails — graph errors are silently swallowed.
func graphRelatedHint(ctx context.Context, st *store.Store, relPath string) string {
	neighbors, err := st.GraphNeighborFiles(ctx, []string{relPath}, 8)
	if err != nil || len(neighbors) == 0 {
		return ""
	}
	return "\n# Related (call graph): " + strings.Join(neighbors, ", ") + "\n"
}

// inlineTaskSymbol appends the body of the symbol most relevant to the active
// session task (if any) to content, so a task-focused read surfaces the code
// that matters even under a compressed mode. Moved here from the removed
// server_compose.go (#429) — it is the only live consumer.
func inlineTaskSymbol(ctx context.Context, st *store.Store, data []byte, syms []store.GraphSymbol, content string) string {
	sess, ok, err := st.SessionGet(ctx)
	if err != nil || !ok || sess.Task == "" {
		return content
	}
	queryTokens := tokenizeWords(sess.Task)
	if len(queryTokens) == 0 {
		return content
	}
	var bestSym store.GraphSymbol
	bestScore := 0
	for _, sym := range syms {
		if sc := symbolQueryScore(queryTokens, sym); sc > bestScore {
			bestScore = sc
			bestSym = sym
		}
	}
	if bestScore == 0 || data == nil {
		return content
	}
	endLine := bestSym.EndLine
	if endLine-bestSym.StartLine > 60 {
		endLine = bestSym.StartLine + 59
	}
	body, sLine, eLine := SliceLines(data, bestSym.StartLine, endLine)
	if len(body) == 0 {
		return content
	}
	return content + fmt.Sprintf("\n# Task-relevant: %s %s (lines %d-%d)\n```\n%s```\n",
		bestSym.Kind, bestSym.QualifiedName, sLine, eLine, string(body))
}

// symbolQueryScore counts token overlap between the query and the symbol's
// qualified name tokens. 0 means no overlap.
func symbolQueryScore(queryTokens []string, sym store.GraphSymbol) int {
	symTokens := tokenizeWords(sym.QualifiedName)
	score := 0
	for _, qt := range queryTokens {
		for _, st := range symTokens {
			if qt == st {
				score++
			}
		}
	}
	return score
}

// tokenizeWords splits text into lowercase tokens (length > 2) breaking on
// non-alphanumeric characters and camelCase boundaries.
func tokenizeWords(s string) []string {
	var tokens []string
	var cur strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
			cur.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			if cur.Len() > 2 {
				tokens = append(tokens, cur.String())
			}
			cur.Reset()
			cur.WriteRune(r + 32) // toLower
		case r >= '0' && r <= '9':
			cur.WriteRune(r)
		default:
			if cur.Len() > 2 {
				tokens = append(tokens, cur.String())
			}
			cur.Reset()
		}
	}
	if cur.Len() > 2 {
		tokens = append(tokens, cur.String())
	}
	return tokens
}
