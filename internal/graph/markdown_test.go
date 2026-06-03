package graph

import (
	"context"
	"strings"
	"testing"
)

func TestExtractMarkdownVault(t *testing.T) {
	root := copyFixture(t, "md_vault")
	res, err := ExtractMarkdown(context.Background(), root)
	if err != nil {
		t.Fatalf("ExtractMarkdown: %v", err)
	}

	// One document node per markdown file.
	wantDocs := []string{
		"index.md",
		"specs/auth.md",
		"specs/glossary.md",
		"specs/dup.md",
		"notes/ideas.md",
		"notes/dup.md",
	}
	for _, qn := range wantDocs {
		if findNode(res.Nodes, NodeDocument, qn) == nil {
			t.Errorf("missing document node %s; docs=%v", qn, nodesOfKind(res.Nodes, NodeDocument))
		}
	}

	doc := func(qn string) string { return NodeID("", mdPkg, NodeDocument, qn) }

	// Inline links, including the anchored variant (#flow) and the
	// relative-up resolution (specs/auth.md → ../index.md).
	wantLinks := [][2]string{
		{"index.md", "specs/auth.md"},
		{"index.md", "notes/ideas.md"},
		{"specs/auth.md", "index.md"},
	}
	for _, e := range wantLinks {
		if findEdge(res.Edges, EdgeLinks, doc(e[0]), doc(e[1])) == nil {
			t.Errorf("missing links edge %s → %s", e[0], e[1])
		}
	}

	// Wikilinks: bare, aliased, and the reverse pair (backlink-by-direction).
	wantWiki := [][2]string{
		{"index.md", "specs/glossary.md"},   // [[glossary]]
		{"index.md", "specs/auth.md"},       // [[auth|Authentication]]
		{"specs/auth.md", "notes/ideas.md"}, // [[ideas]]
		{"notes/ideas.md", "specs/auth.md"}, // [[auth]] — reverse direction = backlink
	}
	for _, e := range wantWiki {
		if findEdge(res.Edges, EdgeWikilinks, doc(e[0]), doc(e[1])) == nil {
			t.Errorf("missing wikilinks edge %s → %s", e[0], e[1])
		}
	}

	// Fenced code block link [fake](specs/glossary.md) must NOT emit a
	// links edge (only the [[glossary]] wikilink reaches glossary).
	if findEdge(res.Edges, EdgeLinks, doc("index.md"), doc("specs/glossary.md")) != nil {
		t.Errorf("fenced-code link to specs/glossary.md leaked a links edge")
	}

	// Images, external URLs, and embeds (![[diagram]]) emit nothing.
	if findNode(res.Nodes, NodeDocument, "assets/logo.png") != nil {
		t.Errorf("image target leaked a document node")
	}
	for _, e := range res.Edges {
		if strings.Contains(e.DstID, "logo.png") || strings.Contains(e.DstID, "diagram") {
			t.Errorf("unexpected edge to image/embed: %+v", e)
		}
	}

	// Broken link and ambiguous wikilink are surfaced as warnings, not edges.
	if findEdge(res.Edges, EdgeLinks, doc("index.md"), doc("specs/missing.md")) != nil {
		t.Errorf("broken link specs/missing.md emitted an edge; should warn + skip")
	}
	var sawBroken, sawAmbiguous bool
	for _, w := range res.Warnings {
		if strings.Contains(w, "broken link to specs/missing.md") {
			sawBroken = true
		}
		if strings.Contains(w, "ambiguous wikilink [[dup]]") {
			sawAmbiguous = true
		}
	}
	if !sawBroken {
		t.Errorf("expected a broken-link warning; warnings=%v", res.Warnings)
	}
	if !sawAmbiguous {
		t.Errorf("expected an ambiguous-wikilink warning; warnings=%v", res.Warnings)
	}
}

func TestExtractMarkdownEmbedsAndTags(t *testing.T) {
	root := copyFixture(t, "md_vault")
	res, err := ExtractMarkdown(context.Background(), root)
	if err != nil {
		t.Fatalf("ExtractMarkdown: %v", err)
	}
	doc := func(qn string) string { return NodeID("", mdPkg, NodeDocument, qn) }

	// Transclusions: ![[glossary]] (wiki style) and ![inc](../specs/glossary.md)
	// (relative style) both from notes/ideas.md → specs/glossary.md.
	if findEdge(res.Edges, EdgeTransclude, doc("notes/ideas.md"), doc("specs/glossary.md")) == nil {
		t.Errorf("missing transcludes edge notes/ideas.md → specs/glossary.md")
	}
	// The wiki-style and relative-style embeds sit on different lines, so
	// they are two distinct transclusion edges.
	var transcludeCount int
	for _, e := range res.Edges {
		if e.Kind == EdgeTransclude && e.SrcID == doc("notes/ideas.md") && e.DstID == doc("specs/glossary.md") {
			transcludeCount++
		}
	}
	if transcludeCount != 2 {
		t.Errorf("transcludes ideas.md → glossary.md = %d, want 2 (wiki + relative)", transcludeCount)
	}

	// Tag nodes exist, including a nested tag.
	for _, tag := range []string{"spec", "project/dex"} {
		if findNode(res.Nodes, NodeTag, tag) == nil {
			t.Errorf("missing tag node %q; tags=%v", tag, nodesOfKind(res.Nodes, NodeTag))
		}
	}
	// The ATX heading "# Ideas" must NOT become a tag node.
	if findNode(res.Nodes, NodeTag, "Ideas") != nil {
		t.Errorf("heading '# Ideas' leaked a tag node")
	}

	// EdgeTagged ideas.md → #spec, deduped to ONE edge despite two mentions.
	specTag := NodeID("", mdPkg, NodeTag, "spec")
	var taggedSpec int
	for _, e := range res.Edges {
		if e.Kind == EdgeTagged && e.SrcID == doc("notes/ideas.md") && e.DstID == specTag {
			taggedSpec++
		}
	}
	if taggedSpec != 1 {
		t.Errorf("tagged edges ideas.md → #spec = %d, want 1 (deduped)", taggedSpec)
	}

	// Tags are NOT documents and carry no doc-link edges.
	if findEdge(res.Edges, EdgeLinks, doc("notes/ideas.md"), specTag) != nil {
		t.Errorf("tag surfaced as a links edge")
	}
}

func TestExtractMarkdownHeadings(t *testing.T) {
	root := copyFixture(t, "md_vault")
	res, err := ExtractMarkdown(context.Background(), root)
	if err != nil {
		t.Fatalf("ExtractMarkdown: %v", err)
	}
	doc := func(qn string) string { return NodeID("", mdPkg, NodeDocument, qn) }
	heading := func(qn string) string { return NodeID("", mdPkg, NodeHeading, qn) }

	// Heading nodes carry `relpath#slug` qualified names.
	for _, qn := range []string{"specs/auth.md#auth-spec", "specs/auth.md#flow", "notes/deep.md#deep"} {
		if findNode(res.Nodes, NodeHeading, qn) == nil {
			t.Errorf("missing heading node %s; headings=%v", qn, nodesOfKind(res.Nodes, NodeHeading))
		}
	}
	// contains edge: document → its heading.
	if findEdge(res.Edges, EdgeContains, doc("specs/auth.md"), heading("specs/auth.md#flow")) == nil {
		t.Errorf("missing contains edge specs/auth.md → #flow")
	}

	// Anchored inline link (GitHub slug) resolves to the heading node, not
	// the document: index.md line 4 `[auth flow](specs/auth.md#flow)`.
	if findEdge(res.Edges, EdgeLinks, doc("index.md"), heading("specs/auth.md#flow")) == nil {
		t.Errorf("anchored link index.md → specs/auth.md#flow did not resolve to the heading")
	}
	// The non-anchored link on line 3 still targets the document.
	if findEdge(res.Edges, EdgeLinks, doc("index.md"), doc("specs/auth.md")) == nil {
		t.Errorf("plain link index.md → specs/auth.md (doc) missing")
	}

	// deep.md: good anchor → heading; literal Obsidian anchor → heading
	// (via lowercased heading text); bad anchor → falls back to the doc.
	if findEdge(res.Edges, EdgeLinks, doc("notes/deep.md"), heading("specs/auth.md#flow")) == nil {
		t.Errorf("deep.md good anchor did not resolve to #flow heading")
	}
	if findEdge(res.Edges, EdgeWikilinks, doc("notes/deep.md"), heading("specs/auth.md#auth-spec")) == nil {
		t.Errorf("deep.md literal wiki anchor [[auth#Auth Spec]] did not resolve to #auth-spec heading")
	}
	if findEdge(res.Edges, EdgeLinks, doc("notes/deep.md"), doc("specs/auth.md")) == nil {
		t.Errorf("deep.md bad anchor did not fall back to the specs/auth.md document")
	}
}

func TestExtractMarkdownNoMarkdown(t *testing.T) {
	root := copyFixture(t, "simple") // Go fixture, no .md files
	res, err := ExtractMarkdown(context.Background(), root)
	if err != nil {
		t.Fatalf("ExtractMarkdown: %v", err)
	}
	if len(res.Nodes) != 0 || len(res.Edges) != 0 {
		t.Errorf("expected empty result on a markdown-free tree; got nodes=%d edges=%d",
			len(res.Nodes), len(res.Edges))
	}
}
