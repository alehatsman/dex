package chunk

import (
	"context"
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
)

// extractiveSigLines caps how many leading lines a multi-line signature
// (wrapped parameter lists, generic bounds) may span before we give up
// and treat the accumulated lines as the signature.
const extractiveSigLines = 8

// ExtractiveSummary distills a structural chunk down to the three parts
// that carry the identifiers a retrieval query matches on, at zero GPU
// cost: the leading doc comment, the declaration signature, and the first
// line of the body.
//
// It re-parses the chunk's own source with tree-sitter (selected by the
// path extension) to locate the declaration's `body` field — the standard
// field name every grammar we target exposes — so the signature boundary
// and first statement are exact rather than brace-counted. The original
// per-file parse can't be reused here: the deferred drain path rebuilds a
// chunk from stored text with no live node, and re-parsing a small fragment
// is microseconds of CPU versus the second-long GPU chat call it replaces.
//
// Fragments that don't re-parse into a body-bearing declaration (a bare
// nested method lifted out of its class, an unsupported extension, a
// bodyless `type`/`interface` decl) fall back to textExtractive, a textual
// heuristic over the content's line layout.
func ExtractiveSummary(ctx context.Context, c Chunk) string {
	if s, ok := treeExtractive(ctx, c); ok {
		return s
	}
	return textExtractive(c)
}

// treeExtractive does the tree-sitter-backed extraction. It returns ok=false
// (signalling a textual fallback) when the chunk's language is unsupported,
// the fragment fails to parse, or the declaration has no `body` field.
func treeExtractive(ctx context.Context, c Chunk) (string, bool) {
	cfg, ok := languages[strings.ToLower(filepath.Ext(c.Path))]
	if !ok {
		return "", false
	}
	src := []byte(c.Content)
	parser := sitter.NewParser()
	defer parser.Close()
	parser.SetLanguage(cfg.lang)
	tree, err := parser.ParseCtx(ctx, nil, src)
	if err != nil {
		return "", false
	}
	defer tree.Close()

	decl := firstDecl(tree.RootNode())
	if decl == nil {
		return "", false
	}
	body := decl.ChildByFieldName("body")
	if body == nil {
		return "", false
	}

	var parts []string
	// Doc: everything before the declaration is the backfilled leading
	// comment block (the decl node itself starts after the comments).
	if doc := strings.TrimRight(strings.TrimLeft(string(src[:decl.StartByte()]), "\n"), " \t\n"); doc != "" {
		parts = append(parts, doc)
	}
	// Signature: declaration start up to the body's opening — exact, so a
	// string literal containing a brace or a wrapped generic bound can't
	// throw it off.
	if sig := strings.TrimSpace(string(src[decl.StartByte():body.StartByte()])); sig != "" {
		parts = append(parts, sig)
	}
	// First body statement, first line only. Skip leading comment nodes so
	// we surface real code (or a docstring) rather than an inner comment.
	if first := firstBodyLine(body, src); first != "" {
		parts = append(parts, first)
	}
	if len(parts) == 0 {
		return "", false
	}
	return strings.Join(parts, "\n"), true
}

// firstDecl returns the first non-comment named child of root — the
// declaration the chunk represents — or nil if there is none.
func firstDecl(root *sitter.Node) *sitter.Node {
	for i := range int(root.NamedChildCount()) {
		n := root.NamedChild(i)
		if n == nil || strings.Contains(n.Type(), "comment") {
			continue
		}
		return n
	}
	return nil
}

// firstBodyLine returns the first line of the body's first non-comment
// statement, right-trimmed. Empty when the body has no statements.
func firstBodyLine(body *sitter.Node, src []byte) string {
	for i := range int(body.NamedChildCount()) {
		n := body.NamedChild(i)
		if n == nil || strings.Contains(n.Type(), "comment") {
			continue
		}
		stmt := string(src[n.StartByte():n.EndByte()])
		line, _, _ := strings.Cut(stmt, "\n")
		return strings.TrimRight(line, " \t")
	}
	return ""
}

// textExtractive is the fallback for chunks treeExtractive can't handle. It
// reconstructs doc + signature + first body line from the content's line
// layout:
//
//	// leading doc comment        ← doc block (contiguous comment lines)
//	// (more comment)
//	func F(a int) (int, error) {  ← signature (up to the body opener)
//	    first := real()           ← first body line
//	    ...
//	}
//
// The signature run extends across wrapped lines until parenthesis/bracket
// nesting closes (so multi-line parameter lists survive); decorator lines
// (`@…`) are absorbed without terminating it. Languages without a brace or
// `:` opener (Ruby, Lua) simply yield a one-line signature, which is
// correct for them. The result is joined with newlines and never exceeds
// the original content.
func textExtractive(c Chunk) string {
	lines := strings.Split(c.Content, "\n")
	var out []string
	i := 0

	// 1. Leading doc comment: the contiguous run of comment lines that
	//    backfillComments pulled in above the declaration.
	for i < len(lines) && hasCommentPrefix([]byte(strings.TrimSpace(lines[i]))) {
		out = append(out, strings.TrimRight(lines[i], " \t"))
		i++
	}
	// Skip any blank lines between the doc block and the declaration.
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}

	// 2. Signature: declaration lines up to and including the one that
	//    opens the body. Parenthesis/bracket depth tracks wrapped
	//    parameter lists; a decorator line keeps the run going.
	depth := 0
	start := i
	for i < len(lines) && i-start < extractiveSigLines {
		line := lines[i]
		out = append(out, strings.TrimRight(line, " \t"))
		i++
		depth += parenDelta(line)
		if strings.HasPrefix(strings.TrimSpace(line), "@") {
			continue // decorator/annotation — signature continues below
		}
		if depth <= 0 {
			break // params closed → end of signature
		}
	}

	// 3. First body line: the first line after the signature with real
	//    content (skip blanks, lone punctuation like `{`, and comments).
	for i < len(lines) {
		t := strings.TrimSpace(lines[i])
		if t == "" || isPunctOnly(t) || hasCommentPrefix([]byte(t)) {
			i++
			continue
		}
		out = append(out, strings.TrimRight(lines[i], " \t"))
		break
	}

	return strings.Join(out, "\n")
}

// parenDelta returns the net change in (…) and […] nesting across line,
// ignoring braces — `{` opens a body, not a parameter list, so it must
// not keep the signature run alive.
func parenDelta(line string) int {
	d := 0
	for _, r := range line {
		switch r {
		case '(', '[':
			d++
		case ')', ']':
			d--
		}
	}
	return d
}

// isPunctOnly reports whether s consists solely of structural punctuation
// (braces, parens, semicolons, commas) — e.g. a lone `{` on its own line
// in Allman brace style, which carries no identifier worth keeping.
func isPunctOnly(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch r {
		case '{', '}', '(', ')', ';', ',':
		default:
			return false
		}
	}
	return true
}
