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
	"strconv"
	"strings"

	"github.com/alehatsman/dex/internal/chunk"
	"github.com/alehatsman/dex/internal/store"
)

// ParseLinesRange parses "N-M" from a lines:N-M mode string.
func ParseLinesRange(s string) (start, end int, ok bool) {
	i := strings.IndexByte(s, '-')
	if i <= 0 {
		return 0, 0, false
	}
	n, err1 := strconv.Atoi(s[:i])
	m, err2 := strconv.Atoi(s[i+1:])
	if err1 != nil || err2 != nil || n < 1 || m < n {
		return 0, 0, false
	}
	return n, m, true
}

// SignaturesView builds the mode=signatures view: a compact symbol index,
// followed by a call-graph related-files hint and any task-relevant symbol
// body. This is the whole recipe for the view — callers supply the file bytes
// and indexed symbols (and a store for the graph/session lookups) and get the
// composed text back.
func SignaturesView(ctx context.Context, st *store.Store, data []byte, syms []store.GraphSymbol, relPath string) string {
	content := formatSignatures(data, syms, relPath, nil)
	if related := graphRelatedHint(ctx, st, relPath); related != "" {
		content += related
	}
	return inlineTaskSymbol(ctx, st, data, syms, content)
}

// MapView builds the mode=map view: imports + exported symbols, followed by a
// related-files hint and (when symbols exist) any task-relevant symbol body.
func MapView(ctx context.Context, st *store.Store, data []byte, syms []store.GraphSymbol, imports []string, relPath string) string {
	content := formatMap(relPath, syms, imports)
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
func formatSignatures(src []byte, syms []store.GraphSymbol, relPath string, _ []string) string {
	srcLines := bytes.Split(bytes.TrimRight(src, "\n"), []byte("\n"))
	totalLines := bytes.Count(src, []byte("\n")) + 1
	var b strings.Builder
	fmt.Fprintf(&b, "%s %dL (%d symbols)\n\n", relPath, totalLines, len(syms))

	isTypeKind := func(kind string) bool {
		return kind == "struct" || kind == "interface" || kind == "type"
	}
	// Only top-level named symbols (func/type/var/const) count as exported,
	// not struct fields, imports, or file-level nodes.
	exported := func(sym store.GraphSymbol) bool {
		if sym.Kind == "field" || sym.Kind == "import" || sym.Kind == "file" {
			return false
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

// formatMap produces a compact dependency map for a file: its package-level
// imports and exported declarations, sourced from the index (no LLM, no file
// read). Unexported symbols are omitted so the output mirrors the public API.
func formatMap(relPath string, syms []store.GraphSymbol, imports []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "FILE: %s\n\n", relPath)
	if len(imports) > 0 {
		b.WriteString("IMPORTS:\n")
		for _, imp := range imports {
			fmt.Fprintf(&b, "  %s\n", imp)
		}
		b.WriteByte('\n')
	}
	var exportedLines strings.Builder
	count := 0
	for _, sym := range syms {
		if len(sym.Name) == 0 || sym.Name[0] < 'A' || sym.Name[0] > 'Z' {
			continue
		}
		fmt.Fprintf(&exportedLines, "  %s %s (lines %d-%d)\n", sym.Kind, sym.QualifiedName, sym.StartLine, sym.EndLine)
		count++
	}
	if count > 0 {
		fmt.Fprintf(&b, "EXPORTS (%d):\n", count)
		b.WriteString(exportedLines.String())
	}
	return b.String()
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
		end = line
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
