// Package chunk splits a source file into retrieval-sized chunks.
//
// For languages with a tree-sitter grammar we extract top-level
// declarations (functions, methods, classes, types). Gaps between
// structural chunks are packed using cAST-style greedy sibling-merge:
// consecutive root-level AST siblings are merged into MaxBytes-bounded
// units rather than arbitrary line windows, producing semantically
// coherent orphan chunks. For unknown languages (or when tree-sitter
// fails to parse), we fall back to a fixed-line sliding window.
//
// Chunks are capped at MaxBytes. Anything larger is split into
// MaxBytes-bounded line slices.
package chunk

import (
	"bytes"
	"context"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	sitter "github.com/smacker/go-tree-sitter"
)

// MaxBytes is the upper bound on a single chunk's content (excluding
// the path/kind prefix added at embed time). Roughly 1024 tokens
// assuming the typical 4 chars/token ratio.
const MaxBytes = 4096

// WindowLines is the line-window fallback size.
const WindowLines = 40

// WindowOverlap lines repeat between consecutive line windows.
const WindowOverlap = 10

// Kind values for Chunk.Kind.
const (
	KindWindow = "window"
	KindOrphan = "orphan"
)

// LineCount returns the number of lines in data. A trailing newline is
// treated as a line terminator, not the start of an empty line, so a
// typical POSIX file ending in '\n' reports the same count as an editor
// would show.
func LineCount(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	n := 1
	for _, b := range data {
		if b == '\n' {
			n++
		}
	}
	if data[len(data)-1] == '\n' {
		n--
	}
	return n
}

// Chunk is one retrievable slice of one file.
type Chunk struct {
	Path      string // relative to project root
	Kind      string // tree-sitter node kind or "window"
	Name      string // primary identifier (function/method/type name); empty for windows/orphans
	Parent    string // enclosing class/struct/impl name; empty for top-level chunks
	StartLine int    // 1-based, inclusive
	EndLine   int    // 1-based, inclusive
	Content   string

	// startByte/endByte mark the byte range this chunk covers in the
	// original source. Used internally to compute orphan windows (the
	// gaps between structural chunks); not persisted to the store.
	startByte int
	endByte   int
}

// Chunks splits the given source into chunks. relPath is used only to
// pick the language by extension and is stamped into each Chunk.
//
// For tree-sitter-supported languages we emit one chunk per recognized
// structural declaration AND additional "orphan" chunks covering byte
// ranges not claimed by a structural chunk. Orphan regions are packed
// with cAST-style greedy sibling-merge: consecutive root-level AST
// siblings are merged into MaxBytes-bounded units, producing
// semantically coherent chunks (e.g. a whole import block or const
// group) rather than arbitrary line windows. Without the hybrid pass,
// a file like `package foo; const X = 1; func F(){}` would only index
// F and silently drop X.
func Chunks(ctx context.Context, relPath string, src []byte) ([]Chunk, error) {
	ext := strings.ToLower(filepath.Ext(relPath))
	if cfg, ok := languages[ext]; ok {
		parser := sitter.NewParser()
		defer parser.Close()
		parser.SetLanguage(cfg.lang)
		tree, err := parser.ParseCtx(ctx, nil, src)
		if err == nil {
			defer tree.Close()
			root := tree.RootNode()
			structural := extractStructural(relPath, src, cfg, root)
			if len(structural) > 0 {
				orphans := orphanWindowsAST(relPath, src, root, structural)
				return append(structural, orphans...), nil
			}
			// No structural chunks (e.g. file is only imports/consts) —
			// try AST sibling-merge over the whole file before giving up.
			if packed := astSiblingMerge(relPath, src, root, 0, len(src)); len(packed) > 0 {
				return packed, nil
			}
		}
		// tree-sitter errored or produced nothing usable — fall through.
	}
	return windowChunks(relPath, src), nil
}

// extractStructural returns one chunk per recognized structural
// declaration (functions, methods, types, classes) found in root's
// direct children. It is the former body of treeChunks, now split out
// so Chunks can parse the AST once and reuse the root node for both
// structural extraction and orphan packing.
func extractStructural(relPath string, src []byte, cfg langConfig, root *sitter.Node) []Chunk {
	var out []Chunk
	for i := range int(root.NamedChildCount()) {
		n := root.NamedChild(i)
		if n == nil {
			continue
		}
		kind := n.Type()
		if !cfg.kinds[kind] {
			continue
		}
		startByte := int(n.StartByte())
		endByte := int(n.EndByte())
		// Walk back to include leading line comments / docstrings.
		startByte = backfillComments(src, startByte)
		body := string(src[startByte:endByte])
		startLine := lineOf(src, startByte)
		endLine := max(lineOf(src, endByte-1), startLine)
		name := nodeIdentifier(n, src)
		if len(body) <= MaxBytes {
			out = append(out, Chunk{
				Path: relPath, Kind: kind, Name: name,
				StartLine: startLine, EndLine: endLine,
				Content:   body,
				startByte: startByte,
				endByte:   endByte,
			})
		} else {
			// Oversized declaration → fall back to line windows over its body.
			bodyLines := strings.Split(body, "\n")
			for _, w := range windowOver(bodyLines, startLine) {
				w.Path = relPath
				w.Kind = kind + ":window"
				w.Name = name
				w.startByte = startByte
				w.endByte = endByte
				out = append(out, w)
			}
		}
		// For container kinds (class, impl, trait, module), also extract
		// nested method chunks so each method gets its own index entry with
		// the parent name stamped in EmbedText for richer retrieval.
		if methodKinds, ok := containerMethods[kind]; ok {
			out = append(out, nestedChunks(relPath, src, n, methodKinds, name)...)
		}
	}
	return out
}

// nestedChunks walks one node looking for method-level children and
// returns a Chunk per match. It descends one level of wrapper nodes
// (body, class_body, block) to reach the actual method nodes.
func nestedChunks(relPath string, src []byte, container *sitter.Node, methodKinds map[string]bool, parentName string) []Chunk {
	var out []Chunk
	collectMethods(relPath, src, container, methodKinds, parentName, 2, &out)
	return out
}

func collectMethods(relPath string, src []byte, n *sitter.Node, methodKinds map[string]bool, parentName string, depth int, out *[]Chunk) {
	if depth <= 0 {
		return
	}
	for i := range int(n.NamedChildCount()) {
		child := n.NamedChild(i)
		if child == nil {
			continue
		}
		if methodKinds[child.Type()] {
			if c, ok := buildNestedChunk(relPath, src, child, parentName); ok {
				*out = append(*out, c)
			}
		} else {
			collectMethods(relPath, src, child, methodKinds, parentName, depth-1, out)
		}
	}
}

func buildNestedChunk(relPath string, src []byte, n *sitter.Node, parentName string) (Chunk, bool) {
	startByte := int(n.StartByte())
	endByte := int(n.EndByte())
	startByte = backfillComments(src, startByte)
	body := string(src[startByte:endByte])
	if strings.TrimSpace(body) == "" {
		return Chunk{}, false
	}
	startLine := lineOf(src, startByte)
	endLine := max(lineOf(src, endByte-1), startLine)
	name := nodeIdentifier(n, src)
	if len(body) > MaxBytes {
		// Walk back to the nearest valid UTF-8 rune boundary so we don't
		// split a multi-byte character and hand invalid UTF-8 to the embedder.
		i := MaxBytes
		for i > 0 && !utf8.RuneStart(body[i]) {
			i--
		}
		body = body[:i]
		// Recompute EndLine: it must reflect the truncated content, not the
		// full node extent (which might be hundreds of lines further down).
		if i > 0 {
			endLine = max(lineOf(src, startByte+i-1), startLine)
		} else {
			endLine = startLine
		}
	}
	return Chunk{
		Path: relPath, Kind: n.Type(), Name: name, Parent: parentName,
		StartLine: startLine, EndLine: endLine,
		Content:   body,
		startByte: startByte,
		endByte:   endByte,
	}, true
}

// nodeIdentifier extracts the primary identifier of a tree-sitter node by
// looking up its "name" field — the standard field name for the declared
// identifier in every tree-sitter grammar we target (Go functions, Python
// defs, JS/TS classes, Rust items, Java methods, etc.). Returns "" when the
// node has no such field (e.g. impl_item, lexical_declaration).
//
// Go's `type_declaration` is a wrapper: `type X struct{}` parses as
// type_declaration → type_spec → name:identifier. The wrapper itself
// has no name field. Walk to the first type_spec child so single-spec
// declarations (the overwhelming majority) name their type correctly.
// Multi-spec declarations (`type (X struct{}; Y int)`) still get the
// name of the first spec — better than empty.
//
// Kotlin's class_declaration/function_declaration/object_declaration use
// type_identifier/simple_identifier as the first named child without a
// "name" field. We fall back to that when the field lookup misses.
//
// Elixir's top-level constructs are all `call` nodes; the macro name
// (def/defmodule/defp) sits in an `identifier` child and the declared name
// in the first argument.
func nodeIdentifier(n *sitter.Node, src []byte) string {
	if nameNode := n.ChildByFieldName("name"); nameNode != nil {
		return string(src[nameNode.StartByte():nameNode.EndByte()])
	}
	switch n.Type() {
	case "type_declaration":
		// Go: type_declaration → type_spec/type_alias → name field
		for i := 0; i < int(n.NamedChildCount()); i++ {
			c := n.NamedChild(i)
			if c.Type() != "type_spec" && c.Type() != "type_alias" {
				continue
			}
			if nameNode := c.ChildByFieldName("name"); nameNode != nil {
				return string(src[nameNode.StartByte():nameNode.EndByte()])
			}
		}
	case "call":
		// Elixir: call[identifier=macro][arguments[name|call]]
		// Structure: identifier child holds "def"/"defmodule"/"defp",
		// first argument holds the declared name (alias or nested call).
		return elixirCallName(n, src)
	default:
		// Kotlin (and similar grammars): identifier is the first named child
		// with type type_identifier (class/object) or simple_identifier (fun).
		if n.NamedChildCount() > 0 {
			fc := n.NamedChild(0)
			t := fc.Type()
			if t == "type_identifier" || t == "simple_identifier" {
				return string(src[fc.StartByte():fc.EndByte()])
			}
		}
	}
	return ""
}

// elixirCallName extracts the declared name from an Elixir `call` node.
// defmodule MyModule do  → "MyModule"
// def my_fn(x) do       → "my_fn"
// defp helper(x), do: x → "helper"
func elixirCallName(n *sitter.Node, src []byte) string {
	// Find the macro identifier (first unnamed child of type "identifier").
	var macro string
	for i := 0; i < int(n.ChildCount()); i++ {
		c := n.Child(i)
		if c.Type() == "identifier" {
			macro = string(src[c.StartByte():c.EndByte()])
			break
		}
	}
	if macro == "" {
		return ""
	}
	// Find the arguments node.
	args := n.ChildByFieldName("arguments")
	if args == nil {
		for i := 0; i < int(n.NamedChildCount()); i++ {
			if n.NamedChild(i).Type() == "arguments" {
				args = n.NamedChild(i)
				break
			}
		}
	}
	if args == nil || args.NamedChildCount() == 0 {
		return ""
	}
	first := args.NamedChild(0)
	switch macro {
	case "defmodule":
		// arguments[0] is an alias node like "MyModule"
		if first.Type() == "alias" {
			return string(src[first.StartByte():first.EndByte()])
		}
	case "def", "defp", "defmacro", "defmacrop":
		// arguments[0] is a call node: my_fn(x)
		// The function name is the identifier child of that call.
		if first.Type() == "call" {
			for i := 0; i < int(first.ChildCount()); i++ {
				c := first.Child(i)
				if c.Type() == "identifier" {
					return string(src[c.StartByte():c.EndByte()])
				}
			}
		}
		// arguments[0] might be a bare identifier for zero-arg functions
		if first.Type() == "identifier" {
			return string(src[first.StartByte():first.EndByte()])
		}
	}
	return ""
}

// orphanWindowsAST emits chunks over the parts of src not covered by any
// structural chunk, using cAST-style greedy sibling-merge packing:
// consecutive root-level AST siblings are merged into MaxBytes-bounded
// units rather than arbitrary line windows, producing semantically
// coherent orphan chunks (e.g. a whole import block or const group).
func orphanWindowsAST(relPath string, src []byte, root *sitter.Node, structural []Chunk) []Chunk {
	if len(structural) == 0 {
		return nil
	}
	type iv struct{ s, e int }
	intervals := make([]iv, 0, len(structural))
	for _, c := range structural {
		if c.startByte == 0 && c.endByte == 0 {
			continue
		}
		intervals = append(intervals, iv{c.startByte, c.endByte})
	}
	if len(intervals) == 0 {
		return nil
	}
	sort.Slice(intervals, func(i, j int) bool { return intervals[i].s < intervals[j].s })
	merged := intervals[:1]
	for _, x := range intervals[1:] {
		last := &merged[len(merged)-1]
		if x.s <= last.e {
			if x.e > last.e {
				last.e = x.e
			}
			continue
		}
		merged = append(merged, x)
	}

	var out []Chunk
	cursor := 0
	for _, m := range merged {
		if m.s > cursor {
			out = append(out, astSiblingMerge(relPath, src, root, cursor, m.s)...)
		}
		cursor = m.e
	}
	if cursor < len(src) {
		out = append(out, astSiblingMerge(relPath, src, root, cursor, len(src))...)
	}
	return out
}

// astSiblingMerge greedily packs root-level AST siblings within
// src[start:end] into MaxBytes-bounded orphan chunks (cAST sibling-
// merge). Consecutive sibling nodes are accumulated until adding the
// next one would overflow MaxBytes; each group is emitted as a single
// KindOrphan chunk whose content spans from the first to last node's
// bytes (including any whitespace between them). Leading and trailing
// bytes that lie outside any AST node are absorbed into the adjacent
// group. Falls back to line-window chunking when no AST nodes are
// found in the range.
func astSiblingMerge(relPath string, src []byte, root *sitter.Node, start, end int) []Chunk {
	if start >= end {
		return nil
	}
	if strings.TrimSpace(string(src[start:end])) == "" {
		return nil
	}

	// Collect root-level named children fully contained in [start, end).
	var nodes []*sitter.Node
	for i := range int(root.NamedChildCount()) {
		n := root.NamedChild(i)
		ns, ne := int(n.StartByte()), int(n.EndByte())
		if ns >= start && ne <= end {
			nodes = append(nodes, n)
		}
	}

	// No AST nodes in this range — fall back to line-window chunking.
	if len(nodes) == 0 {
		return orphanRange(relPath, src, start, end)
	}

	var out []Chunk
	// groupStart tracks where the current group begins in src.
	// We start at `start` (not at the first node's startByte) so that
	// any leading bytes (comments, blank lines before the first node)
	// are absorbed into the first group.
	groupStart := start
	groupEnd := start

	flushGroup := func(upTo int) {
		if upTo <= groupStart {
			return
		}
		content := string(src[groupStart:upTo])
		if strings.TrimSpace(content) == "" {
			groupStart = upTo
			groupEnd = upTo
			return
		}
		out = append(out, Chunk{
			Path:      relPath,
			Kind:      KindOrphan,
			StartLine: lineOf(src, groupStart),
			EndLine:   lineOf(src, upTo-1),
			Content:   content,
			startByte: groupStart,
			endByte:   upTo,
		})
		groupStart = upTo
		groupEnd = upTo
	}

	for _, n := range nodes {
		ns, ne := int(n.StartByte()), int(n.EndByte())
		nodeSize := ne - ns

		if nodeSize >= MaxBytes {
			// Single oversized node: flush what we have up to this node,
			// window-chunk the node itself, then continue from its end.
			flushGroup(ns)
			out = append(out, orphanRange(relPath, src, ns, ne)...)
			groupStart = ne
			groupEnd = ne
			continue
		}

		// Would extending the current group to include this node overflow?
		// We measure the projected span from groupStart to ne, which
		// naturally includes any whitespace gap between groupEnd and ns.
		if ne-groupStart > MaxBytes && groupEnd > groupStart {
			flushGroup(groupEnd)
			// groupStart is now groupEnd; fall through to absorb the node.
		}

		groupEnd = ne
	}

	// Absorb trailing bytes (from last node's end to `end`) into the
	// final group, then flush.
	flushGroup(end)

	return out
}

// orphanRange window-chunks src[start:end], stamping chunks with the
// caller's path and kind="orphan". Empty/whitespace-only ranges yield
// no chunks. The line numbers are absolute (1-based) within src.
func orphanRange(relPath string, src []byte, start, end int) []Chunk {
	if start >= end {
		return nil
	}
	slice := string(src[start:end])
	if strings.TrimSpace(slice) == "" {
		return nil
	}
	firstLine := lineOf(src, start)
	lines := strings.Split(slice, "\n")
	wins := windowOver(lines, firstLine)
	for i := range wins {
		wins[i].Path = relPath
		wins[i].Kind = KindOrphan
	}
	return wins
}

func hasCommentPrefix(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	switch b[0] {
	case '/':
		return len(b) >= 2 && (b[1] == '/' || b[1] == '*')
	case '#', '*':
		return true
	case '-':
		return len(b) >= 2 && b[1] == '-'
	}
	return false
}

// backfillComments walks backward from start to absorb a contiguous
// block of leading line comments (`//`, `#`, `--`) or block-comment
// remnants (`/*`, `*`) immediately above the declaration. Limited to
// 50 lines to avoid pulling in unrelated file headers.
//
// `start` must be at the beginning of a line — that is, `src[start-1]`
// is either '\n' or out of range. The function returns a new offset
// that's still at the start of a line.
func backfillComments(src []byte, start int) int {
	pos := start
	lines := 0
	for pos > 0 && lines < 50 {
		// pos points to the start of a line. The previous line ends at
		// pos-1 (a newline, when pos>0) and starts at lineStart where
		// src[lineStart-1] is '\n' or lineStart==0.
		if src[pos-1] != '\n' {
			break
		}
		lineStart := pos - 1 // index of the trailing newline
		// Walk lineStart back to the first byte of the previous line.
		for lineStart > 0 && src[lineStart-1] != '\n' {
			lineStart--
		}
		// src[lineStart:pos-1] is the previous line's content (no newline).
		trimmed := bytes.TrimLeft(src[lineStart:pos-1], " \t")
		if hasCommentPrefix(trimmed) {
			pos = lineStart
			lines++
			continue
		}
		break
	}
	return pos
}

func lineOf(src []byte, byteOffset int) int {
	if byteOffset < 0 {
		byteOffset = 0
	}
	if byteOffset > len(src) {
		byteOffset = len(src)
	}
	return 1 + bytes.Count(src[:byteOffset], []byte{'\n'})
}
