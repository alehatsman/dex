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
// captures an optional leading `!` so image/embed syntax `![alt](src)`
// can be told apart (embeds are slice B). Group 2 is the raw target,
// which may carry a `#anchor` and/or a `"title"` we strip downstream.
var mdInlineLink = regexp.MustCompile(`(!?)\[[^\]]*\]\(([^)]*)\)`)

// mdWikiLink matches an Obsidian-style `[[Note]]` reference. Group 1 is
// the optional leading `!` (transclusion/embed — slice B). Group 2 is
// the inner target, which may carry `#heading` and/or `|alias`.
var mdWikiLink = regexp.MustCompile(`(!?)\[\[([^\]]+)\]\]`)

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

	// pending holds one unresolved reference. For EdgeLinks, target is a
	// relative path (anchor/title stripped) resolved against srcDir after
	// the walk; for EdgeWikilinks, target is the bare note name resolved
	// by basename against the full doc set.
	type pending struct {
		srcFile string // relpath (slash form)
		srcDir  string // dir of srcFile (OS form), for relative resolution
		target  string
		kind    EdgeKind
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
				line:    r.line,
			})
		}
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}

	for _, r := range refs {
		var dst string
		switch r.kind {
		case EdgeLinks:
			resolved, ok := resolveMarkdownRef(projectRoot, r.srcDir, r.target)
			if !ok {
				continue // outside the tree / absolute — silently skipped
			}
			// Only markdown targets join the doc graph. Links to images,
			// code, or other assets are out of scope and skipped without
			// a warning (they aren't broken, just not documents).
			if !markdownExts[strings.ToLower(path.Ext(resolved))] {
				continue
			}
			if _, ok := knownFiles[resolved]; !ok {
				res.Warnings = append(res.Warnings,
					fmt.Sprintf("%s:%d: broken link to %s", r.srcFile, r.line, r.target))
				continue
			}
			dst = resolved
		case EdgeWikilinks:
			resolved, ok, ambiguous := resolveWikilink(r.target, byBasename, knownFiles)
			if ambiguous {
				res.Warnings = append(res.Warnings,
					fmt.Sprintf("%s:%d: ambiguous wikilink [[%s]] (%d matches)",
						r.srcFile, r.line, r.target, len(byBasename[basenameKey(r.target)])))
				continue
			}
			if !ok {
				res.Warnings = append(res.Warnings,
					fmt.Sprintf("%s:%d: unresolved wikilink [[%s]]", r.srcFile, r.line, r.target))
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

// basenameKey is the lowercased, extension-less basename used to key the
// wikilink resolution index. "docs/Spec.md" → "spec".
func basenameKey(p string) string {
	b := path.Base(filepath.ToSlash(p))
	return strings.ToLower(strings.TrimSuffix(b, path.Ext(b)))
}

type mdLink struct {
	kind   EdgeKind
	target string // raw target: relative path (links) or note name (wikilinks)
	line   int
}

// scanMarkdownLinks returns every inline link and wikilink found in the
// file at p. Cheap line scanner — no markdown-parser dependency, mirroring
// scanYAMLRefs. Fenced code blocks are skipped so embedded examples don't
// emit edges; inline code spans are not stripped (a known limitation).
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
			if m[1] == "!" {
				continue // image/embed — slice B
			}
			if tgt := cleanLinkTarget(m[2]); tgt != "" {
				out = append(out, mdLink{kind: EdgeLinks, target: tgt, line: lineno})
			}
		}
		for _, m := range mdWikiLink.FindAllStringSubmatch(line, -1) {
			if m[1] == "!" {
				continue // transclusion/embed — slice B
			}
			if note := cleanWikiTarget(m[2]); note != "" {
				out = append(out, mdLink{kind: EdgeWikilinks, target: note, line: lineno})
			}
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
