package graph

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/alehatsman/dex/internal/ignore"
)

// mdPkg is the synthetic "package" namespacing markdown-graph IDs away
// from Go- and YAML-graph rows in the same store. Markdown files have
// no package, so we group them under a fixed sentinel — same trick as
// yamlPkg.
const mdPkg = "_markdown"

// markdownExts are the file extensions treated as markdown documents.
// .rst/.txt are indexed as chunks (ignore.IndexableExtensions) but don't
// carry markdown link syntax, so they stay out of the doc graph.
var markdownExts = map[string]bool{".md": true, ".markdown": true}

// mdInlineLink matches a markdown inline link `[text](target)`. Group 1
// captures an optional leading `!`: `![alt](src)` is an image/file embed
// (→ EdgeTransclude) rather than a plain link. Group 2 is the raw target,
// which may carry a `#anchor` and/or a `"title"` we strip downstream.
var mdInlineLink = regexp.MustCompile(`(!?)\[[^\]]*\]\(([^)]*)\)`)

// mdWikiLink matches an Obsidian-style `[[Note]]` reference. Group 1 is
// the optional leading `!` marking a transclusion `![[Note]]` (→
// EdgeTransclude). Group 2 is the inner target, which may carry
// `#heading` and/or `|alias`.
var mdWikiLink = regexp.MustCompile(`(!?)\[\[([^\]]+)\]\]`)

// mdTag matches an inline `#tag`. The leading boundary (start-of-line or
// whitespace) keeps it from matching URL fragments (`auth.md#flow`) and
// wikilink headings (`[[Note#h]]`), where `#` is preceded by a word
// char. Requiring a leading letter rejects ATX headings (`# H` has a
// space after `#`) and numeric fragments (`#123`). Nested tags
// (`#area/topic`) are supported via `/`.
var mdTag = regexp.MustCompile(`(?:^|\s)#([A-Za-z][A-Za-z0-9_/-]*)`)

// mdFence matches the opening/closing line of a fenced code block. We
// toggle on it so link-like text inside fences doesn't emit edges.
var mdFence = regexp.MustCompile("^\\s*(```|~~~)")

// ExtractMarkdown walks projectRoot for .md/.markdown files (honoring
// .gitignore + .dexignore via internal/ignore) and emits one document
// node per file plus `links` / `wikilinks` edges between documents.
// Backlinks are not a separate edge kind — they are the reverse
// direction of these edges. Returns an empty ExtractResult on a tree
// with no markdown.
//
// Resolution mirrors ExtractYAML's clean-graph invariant: only edges
// whose target resolves to a document that actually exists are emitted.
// A markdown-looking link that fails to resolve (and ambiguous
// wikilinks) are recorded as Warnings rather than emitted as dangling
// edges — surfacing dangling targets as nodes touches the loadGraphView
// prune contract and is deferred to a follow-up.
func ExtractMarkdown(ctx context.Context, projectRoot string) (*ExtractResult, error) {
	matcher, err := ignore.New(projectRoot)
	if err != nil {
		return nil, fmt.Errorf("ignore.New: %w", err)
	}

	nodeSet := newNodeSet()
	edgeSet := newEdgeSet()
	res := &ExtractResult{}

	// pending holds one unresolved reference. style selects how target is
	// resolved (relative path / vault basename / tag) after the walk, once
	// the full doc set is known; kind is the edge kind to emit.
	type pending struct {
		srcFile string // relpath (slash form)
		srcDir  string // dir of srcFile (OS form), for relative resolution
		target  string
		kind    EdgeKind
		style   refStyle
		line    int
	}
	var refs []pending

	knownFiles := make(map[string]struct{})
	// byBasename maps a lowercased extension-less basename to the docs
	// that carry it, for vault-style wikilink resolution. Multiple
	// entries => ambiguous.
	byBasename := make(map[string][]string)

	walkErr := filepath.WalkDir(projectRoot, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rel, relErr := filepath.Rel(projectRoot, p)
		if relErr != nil {
			return relErr
		}
		if rel == "." {
			return nil
		}
		if matcher.Match(rel, d.IsDir()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !markdownExts[strings.ToLower(filepath.Ext(rel))] {
			return nil
		}

		relSlash := filepath.ToSlash(rel)
		knownFiles[relSlash] = struct{}{}
		base := basenameKey(relSlash)
		byBasename[base] = append(byBasename[base], relSlash)
		nodeSet.add(markdownDocNode(relSlash))

		fileRefs, scanErr := scanMarkdownLinks(p)
		if scanErr != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s: %v", relSlash, scanErr))
			return nil
		}
		dir := filepath.Dir(rel)
		for _, r := range fileRefs {
			refs = append(refs, pending{
				srcFile: relSlash,
				srcDir:  dir,
				target:  r.target,
				kind:    r.kind,
				style:   r.style,
				line:    r.line,
			})
		}
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}

	// seenTag dedups a (document, tag) pair to a single EdgeTagged: a doc
	// that mentions #foo five times relates to the tag once. Links and
	// transclusions stay per-occurrence (distinct line = distinct edge).
	seenTag := make(map[string]struct{})

	for _, r := range refs {
		// Tags resolve to a tag node, not a document.
		if r.style == styleTag {
			key := r.srcFile + "\x00" + r.target
			if _, dup := seenTag[key]; dup {
				continue
			}
			seenTag[key] = struct{}{}
			srcID := NodeID("", mdPkg, NodeDocument, r.srcFile)
			tagNode := markdownTagNode(r.target)
			nodeSet.add(tagNode)
			edgeSet.add(Edge{
				ID:        EdgeID(srcID, EdgeTagged, tagNode.ID, r.srcFile, r.line),
				Kind:      EdgeTagged,
				SrcID:     srcID,
				DstID:     tagNode.ID,
				FilePath:  r.srcFile,
				StartLine: r.line,
				EndLine:   r.line,
			})
			continue
		}

		var dst string
		switch r.style {
		case styleRelative:
			resolved, ok := resolveMarkdownRef(projectRoot, r.srcDir, r.target)
			if !ok {
				continue // outside the tree / absolute — silently skipped
			}
			// Only markdown targets join the doc graph. References to images,
			// code, or other assets are out of scope and skipped without a
			// warning (they aren't broken, just not documents).
			if !markdownExts[strings.ToLower(path.Ext(resolved))] {
				continue
			}
			if _, ok := knownFiles[resolved]; !ok {
				res.Warnings = append(res.Warnings,
					fmt.Sprintf("%s:%d: broken %s to %s", r.srcFile, r.line, refNoun(r.kind), r.target))
				continue
			}
			dst = resolved
		case styleWiki:
			resolved, ok, ambiguous := resolveWikilink(r.target, byBasename, knownFiles)
			if ambiguous {
				res.Warnings = append(res.Warnings,
					fmt.Sprintf("%s:%d: ambiguous %s [[%s]] (%d matches)",
						r.srcFile, r.line, refNoun(r.kind), r.target, len(byBasename[basenameKey(r.target)])))
				continue
			}
			if !ok {
				res.Warnings = append(res.Warnings,
					fmt.Sprintf("%s:%d: unresolved %s [[%s]]", r.srcFile, r.line, refNoun(r.kind), r.target))
				continue
			}
			dst = resolved
		default:
			continue
		}

		if dst == r.srcFile {
			continue // self-link — no edge
		}
		srcID := NodeID("", mdPkg, NodeDocument, r.srcFile)
		dstID := NodeID("", mdPkg, NodeDocument, dst)
		edgeSet.add(Edge{
			ID:        EdgeID(srcID, r.kind, dstID, r.srcFile, r.line),
			Kind:      r.kind,
			SrcID:     srcID,
			DstID:     dstID,
			FilePath:  r.srcFile,
			StartLine: r.line,
			EndLine:   r.line,
		})
	}

	res.Nodes = nodeSet.flatten()
	res.Edges = edgeSet.flatten()
	return res, nil
}

func markdownDocNode(relSlash string) Node {
	return Node{
		ID:            NodeID("", mdPkg, NodeDocument, relSlash),
		Kind:          NodeDocument,
		Name:          filepath.Base(relSlash),
		QualifiedName: relSlash,
		PackagePath:   mdPkg,
		FilePath:      relSlash,
		Metadata:      map[string]any{"language": "markdown"},
	}
}

// markdownTagNode builds the node for a `#tag`. Tags span documents, so
// they carry no FilePath; the tag text is both Name and QualifiedName.
func markdownTagNode(tag string) Node {
	return Node{
		ID:            NodeID("", mdPkg, NodeTag, tag),
		Kind:          NodeTag,
		Name:          tag,
		QualifiedName: tag,
		PackagePath:   mdPkg,
		Metadata:      map[string]any{"language": "markdown"},
	}
}

// refNoun returns the human noun for a reference kind, for warnings.
func refNoun(kind EdgeKind) string {
	switch kind {
	case EdgeTransclude:
		return "embed"
	case EdgeWikilinks:
		return "wikilink"
	default:
		return "link"
	}
}

// basenameKey is the lowercased, extension-less basename used to key the
// wikilink resolution index. "docs/Spec.md" → "spec".
func basenameKey(p string) string {
	b := path.Base(filepath.ToSlash(p))
	return strings.ToLower(strings.TrimSuffix(b, path.Ext(b)))
}

// refStyle is how a reference's target must be resolved, kept separate
// from the edge kind so the same resolution mechanism serves several
// edge semantics (e.g. a relative-path target may be a link OR an
// inline-image transclusion).
type refStyle int

const (
	styleRelative refStyle = iota // resolve target as a path relative to the source dir
	styleWiki                     // resolve target by vault basename
	styleTag                      // target is a tag name; no doc resolution
)

type mdLink struct {
	kind   EdgeKind
	style  refStyle
	target string // relative path (styleRelative), note name (styleWiki), or tag (styleTag)
	line   int
}

// scanMarkdownLinks returns every inline link, wikilink, transclusion,
// and tag found in the file at p. Cheap line scanner — no markdown-parser
// dependency, mirroring scanYAMLRefs. Fenced code blocks are skipped so
// embedded examples don't emit edges; inline code spans are not stripped
// (a known limitation).
func scanMarkdownLinks(p string) ([]mdLink, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var out []mdLink
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 64*1024), 1024*1024)
	lineno := 0
	inFence := false
	for s.Scan() {
		lineno++
		line := s.Text()
		if mdFence.MatchString(line) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		for _, m := range mdInlineLink.FindAllStringSubmatch(line, -1) {
			tgt := cleanLinkTarget(m[2])
			if tgt == "" {
				continue
			}
			// `![alt](x)` is an embed (transclusion); `[t](x)` is a link.
			kind := EdgeLinks
			if m[1] == "!" {
				kind = EdgeTransclude
			}
			out = append(out, mdLink{kind: kind, style: styleRelative, target: tgt, line: lineno})
		}
		for _, m := range mdWikiLink.FindAllStringSubmatch(line, -1) {
			note := cleanWikiTarget(m[2])
			if note == "" {
				continue
			}
			// `![[Note]]` transcludes; `[[Note]]` links.
			kind := EdgeWikilinks
			if m[1] == "!" {
				kind = EdgeTransclude
			}
			out = append(out, mdLink{kind: kind, style: styleWiki, target: note, line: lineno})
		}
		for _, m := range mdTag.FindAllStringSubmatch(line, -1) {
			out = append(out, mdLink{kind: EdgeTagged, style: styleTag, target: m[1], line: lineno})
		}
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// cleanLinkTarget normalises an inline-link target: it unwraps a
// `<...>` destination, drops a trailing `"title"`, strips a `#anchor`,
// and returns "" for same-document anchors and external/absolute URLs
// (which never resolve to a local document node).
func cleanLinkTarget(raw string) string {
	t := strings.TrimSpace(raw)
	if t == "" {
		return ""
	}
	if strings.HasPrefix(t, "<") {
		if i := strings.Index(t, ">"); i >= 0 {
			t = t[1:i]
		}
	} else if i := strings.IndexAny(t, " \t"); i >= 0 {
		t = t[:i] // drop the optional title following whitespace
	}
	if i := strings.Index(t, "#"); i >= 0 {
		t = t[:i] // anchor — resolve to the document, drop the fragment
	}
	t = strings.TrimSpace(t)
	if t == "" {
		return "" // pure same-document anchor
	}
	if strings.Contains(t, "://") || strings.HasPrefix(t, "//") || strings.HasPrefix(t, "mailto:") {
		return "" // external
	}
	return t
}

// cleanWikiTarget strips the `|alias` and `#heading` suffixes from a
// wikilink's inner text, leaving the note reference to resolve.
func cleanWikiTarget(raw string) string {
	t := strings.TrimSpace(raw)
	if i := strings.Index(t, "|"); i >= 0 {
		t = t[:i]
	}
	if i := strings.Index(t, "#"); i >= 0 {
		t = t[:i]
	}
	return strings.TrimSpace(t)
}

// resolveMarkdownRef joins target against the source file's dir and
// confirms the result stays inside projectRoot. Returns the slash-form
// relpath and true on success. Mirrors resolveYAMLRef.
func resolveMarkdownRef(projectRoot, srcDir, target string) (string, bool) {
	if target == "" || filepath.IsAbs(target) {
		return "", false
	}
	joined := filepath.Clean(filepath.Join(srcDir, target))
	abs := filepath.Clean(filepath.Join(projectRoot, joined))
	rel, err := filepath.Rel(projectRoot, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// resolveWikilink resolves a vault-style note reference. A reference
// carrying a path separator or markdown extension is tried as a direct
// relpath first; otherwise it resolves by unique basename match across
// the doc set. Returns (target, ok, ambiguous): ambiguous is true when
// the basename matches more than one document.
func resolveWikilink(note string, byBasename map[string][]string, knownFiles map[string]struct{}) (string, bool, bool) {
	note = filepath.ToSlash(strings.TrimSpace(note))
	if note == "" {
		return "", false, false
	}
	if strings.Contains(note, "/") || markdownExts[strings.ToLower(path.Ext(note))] {
		for _, cand := range []string{note, note + ".md", note + ".markdown"} {
			if _, ok := knownFiles[cand]; ok {
				return cand, true, false
			}
		}
	}
	matches := byBasename[basenameKey(note)]
	switch len(matches) {
	case 0:
		return "", false, false
	case 1:
		return matches[0], true, false
	default:
		return "", false, true
	}
}
