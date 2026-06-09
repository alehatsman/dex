package compress

import (
	"bytes"
	"fmt"
	"strings"
)

// BodyScope describes a named declaration within a source file.
// Used by SkeletonPass to decide what to show vs. suppress.
type BodyScope struct {
	Name      string // qualified name, e.g. "Server.Summarize"
	Kind      string // "function" | "method" | "struct" | "interface" | "type"
	Exported  bool
	StartLine int // 1-indexed, inclusive
	EndLine   int // 1-indexed, inclusive
}

// BodyEntry is a suppressed function/method body available for on-demand
// expansion via file_view expand=@B<n>.
type BodyEntry struct {
	N         int    // handle number matching @B<n> in the skeleton text
	Name      string // qualified function/method name
	StartLine int    // start of the full declaration (1-indexed)
	EndLine   int    // end of the full declaration (1-indexed)
}

// SkeletonResult is the output of SkeletonPass.
type SkeletonResult struct {
	Text   string      // skeleton text with @B<n> placeholders
	Bodies []BodyEntry // one entry per suppressed function/method body
}

// SkeletonPass produces a skeleton view of src:
//   - Exported type/struct/interface declarations are shown in full.
//   - Exported function/method bodies are replaced with @B<n> placeholders.
//   - Unexported functions are omitted (counted in a summary line).
//
// scopes must be sorted by StartLine ascending.
func SkeletonPass(src []byte, path string, scopes []BodyScope) SkeletonResult {
	srcLines := bytes.Split(src, []byte("\n"))
	// Drop trailing empty element when src ends with '\n'.
	if len(srcLines) > 0 && len(srcLines[len(srcLines)-1]) == 0 {
		srcLines = srcLines[:len(srcLines)-1]
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s %dL (%d symbols)\n\n", path, len(srcLines), len(scopes))

	var types, funcs []BodyScope
	unexported := 0
	for _, s := range scopes {
		switch s.Kind {
		case "struct", "interface", "type":
			if s.Exported {
				types = append(types, s)
			}
		case "function", "method":
			if s.Exported {
				funcs = append(funcs, s)
			} else {
				unexported++
			}
		}
	}

	// Exported types first, shown in full.
	for _, s := range types {
		lo := max(s.StartLine-1, 0)
		hi := min(s.EndLine, len(srcLines))
		for i := lo; i < hi; i++ {
			b.Write(srcLines[i])
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	}

	if unexported > 0 {
		fmt.Fprintf(&b, "// %d unexported function(s) omitted\n\n", unexported)
	}

	var bodies []BodyEntry
	n := 1
	for _, s := range funcs {
		lo := max(s.StartLine-1, 0)
		hi := min(s.EndLine-1, len(srcLines)-1)

		braceIdx := skeletonFindOpenBrace(srcLines, lo, hi)

		// Emit lines before the brace line (multi-line signatures).
		for i := lo; i < braceIdx && i < len(srcLines); i++ {
			b.Write(srcLines[i])
			b.WriteByte('\n')
		}
		// Emit the signature portion of the brace line (truncate at '{').
		if braceIdx < len(srcLines) {
			line := srcLines[braceIdx]
			pos := bytes.LastIndexByte(line, '{')
			if pos >= 0 {
				b.Write(bytes.TrimRight(line[:pos], " \t"))
			} else {
				b.Write(line)
			}
		}
		nLines := s.EndLine - s.StartLine + 1
		fmt.Fprintf(&b, " { /* @B%d: %d lines */ }\n\n", n, nLines)

		bodies = append(bodies, BodyEntry{
			N:         n,
			Name:      s.Name,
			StartLine: s.StartLine,
			EndLine:   s.EndLine,
		})
		n++
	}

	if len(bodies) > 0 {
		b.WriteString("// Expand: file_view path=<file> expand=@B<n>\n")
	}

	return SkeletonResult{Text: b.String(), Bodies: bodies}
}

// skeletonFindOpenBrace scans srcLines[startIdx..endIdx] (0-based, inclusive)
// and returns the 0-based index of the line where the function body opens
// (i.e. where brace depth goes 0→1). Returns startIdx if not found.
func skeletonFindOpenBrace(srcLines [][]byte, startIdx, endIdx int) int {
	depth := 0
	for i := startIdx; i <= endIdx && i < len(srcLines); i++ {
		line := srcLines[i]
		for j := 0; j < len(line); j++ {
			c := line[j]
			// Skip line comment remainder.
			if c == '/' && j+1 < len(line) && line[j+1] == '/' {
				break
			}
			// Skip double-quoted string literal.
			if c == '"' {
				j++
				for j < len(line) {
					if line[j] == '\\' {
						j++ // skip escaped char
					} else if line[j] == '"' {
						break
					}
					j++
				}
				continue
			}
			if c == '{' {
				depth++
				if depth == 1 {
					return i
				}
			} else if c == '}' && depth > 0 {
				depth--
			}
		}
	}
	return startIdx
}
