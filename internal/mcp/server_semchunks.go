package mcp

import (
	"strings"
	"unicode"
)

type chunkKind int

const (
	chunkKindImports chunkKind = iota
	chunkKindTypeDef
	chunkKindFuncDef
	chunkKindLogic
	chunkKindEmpty
)

type semChunk struct {
	lines      []string
	kind       chunkKind
	identifier string
	relevance  float64
	startLine  int // 0-indexed within original content
}

// detectSemanticChunks parses source code into structural groups via a
// brace-depth FSM. Contiguous blank lines form a single Empty chunk; import
// blocks become an Imports chunk; struct/type/enum/class declarations become
// TypeDef; function/method bodies become FuncDef; everything else is Logic.
func detectSemanticChunks(content string) []semChunk {
	lines := strings.Split(content, "\n")
	var chunks []semChunk
	i := 0
	for i < len(lines) {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		// Empty run
		if trimmed == "" {
			j := i
			for j < len(lines) && strings.TrimSpace(lines[j]) == "" {
				j++
			}
			chunks = append(chunks, semChunk{
				lines:     lines[i:j],
				kind:      chunkKindEmpty,
				startLine: i,
			})
			i = j
			continue
		}

		// Import block: contiguous lines that start with import/use/from/require
		if isImportLine(trimmed) {
			j := i
			for j < len(lines) {
				t := strings.TrimSpace(lines[j])
				if t == "" || isImportLine(t) || t == "(" || t == ")" {
					j++
					continue
				}
				break
			}
			// trim trailing blank lines from the block
			for j > i && strings.TrimSpace(lines[j-1]) == "" {
				j--
			}
			chunks = append(chunks, semChunk{
				lines:     lines[i:j],
				kind:      chunkKindImports,
				startLine: i,
			})
			i = j
			continue
		}

		// Type/struct/enum/interface definition
		if isTypeDefLine(trimmed) {
			j, ident := consumeBraceBlock(lines, i)
			chunks = append(chunks, semChunk{
				lines:      lines[i:j],
				kind:       chunkKindTypeDef,
				identifier: ident,
				startLine:  i,
			})
			i = j
			continue
		}

		// Function/method definition
		if isFuncDefLine(trimmed) {
			j, ident := consumeBraceBlock(lines, i)
			chunks = append(chunks, semChunk{
				lines:      lines[i:j],
				kind:       chunkKindFuncDef,
				identifier: ident,
				startLine:  i,
			})
			i = j
			continue
		}

		// Logic: collect until we hit a structural boundary or blank run
		j := i + 1
		for j < len(lines) {
			t := strings.TrimSpace(lines[j])
			if t == "" || isImportLine(t) || isTypeDefLine(t) || isFuncDefLine(t) {
				break
			}
			j++
		}
		chunks = append(chunks, semChunk{
			lines:     lines[i:j],
			kind:      chunkKindLogic,
			startLine: i,
		})
		i = j
	}
	return chunks
}

// consumeBraceBlock advances from startLine until brace depth returns to 0,
// returning the exclusive end index and the identifier extracted from the first
// line (e.g. "AuthMiddleware" from "func (s *Server) AuthMiddleware(").
func consumeBraceBlock(lines []string, start int) (end int, ident string) {
	ident = extractIdentifier(strings.TrimSpace(lines[start]))
	depth := 0
	for i := start; i < len(lines); i++ {
		for _, ch := range lines[i] {
			if ch == '{' {
				depth++
			} else if ch == '}' {
				depth--
			}
		}
		if depth <= 0 && i > start {
			return i + 1, ident
		}
	}
	return len(lines), ident
}

// scoreChunks assigns relevance in-place based on keyword overlap and original
// position. keywords should be lowercase.
func scoreChunks(chunks []semChunk, keywords []string) {
	n := len(chunks)
	for i := range chunks {
		c := &chunks[i]
		// base priority by kind
		switch c.kind {
		case chunkKindImports:
			c.relevance = 0.3
		case chunkKindTypeDef:
			c.relevance = 0.4
		case chunkKindFuncDef:
			c.relevance = 0.5
		case chunkKindLogic:
			c.relevance = 0.2
		case chunkKindEmpty:
			c.relevance = 0.0
		}
		// keyword overlap
		body := strings.ToLower(strings.Join(c.lines, "\n"))
		for _, kw := range keywords {
			if strings.Contains(body, kw) {
				c.relevance += 0.5
			}
		}
		// original position attention (L-curve: early lines get a small boost)
		posWeight := 0.0
		if n > 1 {
			frac := float64(i) / float64(n-1) // 0.0 at start, 1.0 at end
			// L-curve: high at 0, low in middle, slightly higher at 1
			if frac < 0.5 {
				posWeight = (0.5 - frac) * 0.2
			} else {
				posWeight = (frac - 0.5) * 0.05
			}
		}
		c.relevance += posWeight
	}
}

// orderForAttention stable-sorts chunks by relevance descending, then moves
// any Imports chunk(s) adjacent to the highest-relevance FuncDef/TypeDef chunk.
// Empty chunks are appended at the end.
func orderForAttention(chunks []semChunk) []semChunk {
	if len(chunks) == 0 {
		return chunks
	}

	// Partition empty from non-empty
	var nonEmpty, empty []semChunk
	for _, c := range chunks {
		if c.kind == chunkKindEmpty {
			empty = append(empty, c)
		} else {
			nonEmpty = append(nonEmpty, c)
		}
	}

	// Stable sort non-empty by relevance desc
	stableSortDesc(nonEmpty)

	// Collect import chunks separately — they always go second (just after
	// the most relevant chunk, so the model sees imports right after context)
	var imports, rest []semChunk
	for _, c := range nonEmpty {
		if c.kind == chunkKindImports {
			imports = append(imports, c)
		} else {
			rest = append(rest, c)
		}
	}

	out := make([]semChunk, 0, len(chunks))
	if len(rest) > 0 {
		out = append(out, rest[0])     // most relevant first
		out = append(out, imports...)  // imports adjacent
		out = append(out, rest[1:]...) // remaining by score (attention dead zone → end)
	} else {
		out = append(out, imports...)
	}
	// empty chunks at end — they carry no signal
	_ = empty
	return out
}

// renderWithBridges joins chunks and appends a tail anchor repeating the
// first 3-5 lines of the highest-relevance FuncDef/TypeDef as a bridge.
func renderWithBridges(ordered []semChunk) string {
	if len(ordered) == 0 {
		return ""
	}
	var sb strings.Builder
	for i, c := range ordered {
		sb.WriteString(strings.Join(c.lines, "\n"))
		if i < len(ordered)-1 {
			sb.WriteByte('\n')
		}
	}

	// Tail anchor: first visible FuncDef or TypeDef
	var anchor *semChunk
	for i := range ordered {
		if ordered[i].kind == chunkKindFuncDef || ordered[i].kind == chunkKindTypeDef {
			anchor = &ordered[i]
			break
		}
	}
	if anchor != nil && anchor.identifier != "" {
		end := 5
		if end > len(anchor.lines) {
			end = len(anchor.lines)
		}
		bridge := strings.TrimRight(strings.Join(anchor.lines[:end], "\n"), " \t")
		sb.WriteString("\n\n// § attention bridge: " + anchor.identifier + "\n")
		sb.WriteString(bridge)
		if end < len(anchor.lines) {
			sb.WriteString("\n// ...")
		}
	}
	return sb.String()
}

// applySemanticChunkOrder is the public entry point. It detects chunks, scores
// them by task keyword overlap, and returns reordered content with bridges.
// Falls back to the original content when fewer than 4 chunks are detected or
// no task keywords are extracted.
func applySemanticChunkOrder(content, task string) string {
	keywords := extractTaskKeywords(task)
	chunks := detectSemanticChunks(content)
	if len(chunks) < 4 || len(keywords) == 0 {
		return content
	}
	scoreChunks(chunks, keywords)
	ordered := orderForAttention(chunks)
	return renderWithBridges(ordered)
}

// extractTaskKeywords lowercases and deduplicates meaningful words from a task
// description, filtering English stop words and short tokens.
func extractTaskKeywords(task string) []string {
	if task == "" {
		return nil
	}
	stop := map[string]bool{
		"a": true, "an": true, "the": true, "and": true, "or": true,
		"in": true, "on": true, "at": true, "to": true, "for": true,
		"of": true, "with": true, "is": true, "are": true, "was": true,
		"be": true, "by": true, "it": true, "as": true, "do": true,
		"from": true, "that": true, "this": true, "not": true, "but": true,
		"all": true, "into": true, "out": true, "its": true, "has": true,
	}
	seen := map[string]bool{}
	var out []string
	for _, word := range strings.FieldsFunc(strings.ToLower(task), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if len(word) < 3 || stop[word] || seen[word] {
			continue
		}
		seen[word] = true
		out = append(out, word)
	}
	return out
}

func isImportLine(s string) bool {
	return strings.HasPrefix(s, "import ") || strings.HasPrefix(s, "import\t") ||
		s == "import" ||
		strings.HasPrefix(s, "from ") ||
		strings.HasPrefix(s, "use ") ||
		strings.HasPrefix(s, "require(") ||
		strings.HasPrefix(s, "require ")
}

func isTypeDefLine(s string) bool {
	return strings.HasPrefix(s, "type ") ||
		strings.HasPrefix(s, "struct ") ||
		strings.HasPrefix(s, "enum ") ||
		strings.HasPrefix(s, "class ") ||
		strings.HasPrefix(s, "interface ") ||
		strings.HasPrefix(s, "pub struct ") ||
		strings.HasPrefix(s, "pub enum ") ||
		strings.HasPrefix(s, "pub class ")
}

func isFuncDefLine(s string) bool {
	return strings.HasPrefix(s, "func ") ||
		strings.HasPrefix(s, "fn ") ||
		strings.HasPrefix(s, "def ") ||
		strings.HasPrefix(s, "async def ") ||
		strings.HasPrefix(s, "pub fn ") ||
		strings.HasPrefix(s, "async fn ") ||
		strings.HasPrefix(s, "pub async fn ") ||
		strings.HasPrefix(s, "impl ") ||
		strings.HasPrefix(s, "function ") ||
		strings.HasPrefix(s, "export function ") ||
		strings.HasPrefix(s, "export default function ") ||
		strings.HasPrefix(s, "export async function ") ||
		strings.HasPrefix(s, "export const ") ||
		(strings.HasPrefix(s, "func(") && !strings.Contains(s, " ")) // anonymous
}

// extractIdentifier extracts the primary name from a declaration line.
// e.g. "func (s *Server) HandleAuth(" → "HandleAuth"
//
//	"type AuthResult struct {"    → "AuthResult"
//	"class Middleware:"           → "Middleware"
func extractIdentifier(line string) string {
	// method receiver: func (s *T) Name(
	if strings.HasPrefix(line, "func (") || strings.HasPrefix(line, "func(") {
		if idx := strings.Index(line, ") "); idx != -1 {
			rest := strings.TrimSpace(line[idx+2:])
			return firstIdent(rest)
		}
	}
	for _, prefix := range []string{
		"func ", "fn ", "def ", "async def ", "pub fn ", "async fn ",
		"pub async fn ", "function ", "export function ", "export async function ",
		"type ", "struct ", "enum ", "class ", "interface ",
		"pub struct ", "pub enum ", "pub class ", "impl ",
		"export const ", "export default function ",
	} {
		if strings.HasPrefix(line, prefix) {
			rest := strings.TrimSpace(line[len(prefix):])
			return firstIdent(rest)
		}
	}
	return firstIdent(line)
}

func firstIdent(s string) string {
	// strip generic brackets, parens, etc.
	for i, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' {
			return s[:i]
		}
	}
	return s
}

func stableSortDesc(chunks []semChunk) {
	// insertion sort — stable, O(n²) acceptable for small slices
	for i := 1; i < len(chunks); i++ {
		for j := i; j > 0 && chunks[j].relevance > chunks[j-1].relevance; j-- {
			chunks[j], chunks[j-1] = chunks[j-1], chunks[j]
		}
	}
}
