package mcp

import (
	"testing"

	"github.com/alehatsman/dex/internal/graph"
)

// docVaultView hand-builds a graphView for a small doc vault:
//
//	index.md  --links-->     specs/auth.md   (two sites: lines 3 and 4)
//	index.md  --wikilinks--> specs/auth.md   (line 8)
//	index.md  --wikilinks--> specs/glossary.md
//	notes/ideas.md --wikilinks--> specs/auth.md
//	index.md  --calls-->     specs/auth.md   (a bogus code edge that must
//	                                           never surface via doc tools)
func docVaultView() *graphView {
	doc := func(rel string) graphNode {
		return graphNode{
			ID:   graph.NodeID("", "_markdown", graph.NodeDocument, rel),
			Kind: graph.NodeDocument, Name: baseName(rel), QualifiedName: rel,
			PackagePath: "_markdown", FilePath: rel,
		}
	}
	index := doc("index.md")
	auth := doc("specs/auth.md")
	gloss := doc("specs/glossary.md")
	ideas := doc("notes/ideas.md")

	edge := func(kind graph.EdgeKind, src, dst graphNode, line int) graphEdge {
		return graphEdge{Kind: kind, SrcID: src.ID, DstID: dst.ID, FilePath: src.FilePath, StartLine: line}
	}
	edges := []graphEdge{
		edge(graph.EdgeLinks, index, auth, 3),
		edge(graph.EdgeLinks, index, auth, 4),
		edge(graph.EdgeWikilinks, index, auth, 8),
		edge(graph.EdgeWikilinks, index, gloss, 7),
		edge(graph.EdgeWikilinks, ideas, auth, 3),
		edge(graph.EdgeCalls, index, auth, 99), // must be ignored by doc tools
	}

	v := &graphView{
		nodesByID:        map[string]graphNode{},
		nodesByQualified: map[string][]graphNode{},
		nodesByPath:      map[string][]graphNode{},
		edgesBySrc:       map[string][]graphEdge{},
		edgesByDst:       map[string][]graphEdge{},
	}
	for _, n := range []graphNode{index, auth, gloss, ideas} {
		v.nodesByID[n.ID] = n
		// Mirror loadGraphView exactly: nodesByQualified is populated only
		// when QualifiedName != Name (so a root-level doc like "index.md"
		// is absent), while nodesByPath is keyed on FilePath for every node.
		if n.QualifiedName != "" && n.QualifiedName != n.Name {
			v.nodesByQualified[n.QualifiedName] = append(v.nodesByQualified[n.QualifiedName], n)
		}
		if n.FilePath != "" {
			v.nodesByPath[n.FilePath] = append(v.nodesByPath[n.FilePath], n)
		}
	}
	for _, e := range edges {
		v.edgesBySrc[e.SrcID] = append(v.edgesBySrc[e.SrcID], e)
		v.edgesByDst[e.DstID] = append(v.edgesByDst[e.DstID], e)
	}
	return v
}

func baseName(rel string) string {
	for i := len(rel) - 1; i >= 0; i-- {
		if rel[i] == '/' {
			return rel[i+1:]
		}
	}
	return rel
}

func TestCollectDocEdgesOutgoing(t *testing.T) {
	v := docVaultView()
	targets := resolveDocTargets(v, "index.md")
	if len(targets) != 1 {
		t.Fatalf("resolveDocTargets(index.md) = %d targets, want 1", len(targets))
	}
	hits := collectDocEdges(v, targets, false, 50)

	// 4 outgoing doc edges (two auth links on distinct lines + one auth
	// wikilink + one glossary wikilink). The `calls` edge is excluded.
	if len(hits) != 4 {
		t.Fatalf("outgoing hits = %d, want 4: %+v", len(hits), hits)
	}
	for _, h := range hits {
		if h.Kind != "links" && h.Kind != "wikilinks" {
			t.Errorf("doc tool surfaced a non-doc edge kind %q", h.Kind)
		}
	}
	// Sorted by doc, then kind, then line: glossary(wiki) last; auth links
	// (lines 3,4) before auth wikilink (line 8).
	want := []DocLink{
		{Doc: "specs/auth.md", Kind: "links", LinkSiteLine: 3},
		{Doc: "specs/auth.md", Kind: "links", LinkSiteLine: 4},
		{Doc: "specs/auth.md", Kind: "wikilinks", LinkSiteLine: 8},
		{Doc: "specs/glossary.md", Kind: "wikilinks", LinkSiteLine: 7},
	}
	for i, w := range want {
		if hits[i].Doc != w.Doc || hits[i].Kind != w.Kind || hits[i].LinkSiteLine != w.LinkSiteLine {
			t.Errorf("hit[%d] = {%s %s :%d}, want {%s %s :%d}",
				i, hits[i].Doc, hits[i].Kind, hits[i].LinkSiteLine, w.Doc, w.Kind, w.LinkSiteLine)
		}
	}
}

func TestCollectDocEdgesBacklinks(t *testing.T) {
	v := docVaultView()
	targets := resolveDocTargets(v, "specs/auth.md")
	hits := collectDocEdges(v, targets, true, 50)

	// Incoming: index.md links (3,4) + index.md wikilink (8) + ideas.md
	// wikilink (3) = 4. The calls edge is excluded.
	if len(hits) != 4 {
		t.Fatalf("backlinks = %d, want 4: %+v", len(hits), hits)
	}
	var fromIdeas bool
	for _, h := range hits {
		if h.Kind == "calls" {
			t.Errorf("backlinks surfaced a calls edge")
		}
		if h.Doc == "notes/ideas.md" {
			fromIdeas = true
		}
	}
	if !fromIdeas {
		t.Errorf("expected a backlink from notes/ideas.md; got %+v", hits)
	}
}

// TestCollectDocEdgesHeadingRollup checks that a link to a section
// (`spec.md#flow`, a heading node) counts as a backlink of spec.md and
// surfaces the anchor, and that outgoing links render a heading peer
// under its parent doc. The doc→heading `contains` edge must not leak in.
func TestCollectDocEdgesHeadingRollup(t *testing.T) {
	mkDoc := func(rel string) graphNode {
		return graphNode{
			ID:   graph.NodeID("", "_markdown", graph.NodeDocument, rel),
			Kind: graph.NodeDocument, Name: baseName(rel), QualifiedName: rel,
			PackagePath: "_markdown", FilePath: rel,
		}
	}
	spec := mkDoc("spec.md")
	guide := mkDoc("guide.md")
	flowQN := "spec.md#flow"
	flow := graphNode{
		ID:   graph.NodeID("", "_markdown", graph.NodeHeading, flowQN),
		Kind: graph.NodeHeading, Name: "Flow", QualifiedName: flowQN,
		PackagePath: "_markdown", FilePath: "spec.md",
	}
	edge := func(kind graph.EdgeKind, src, dst graphNode, line int) graphEdge {
		return graphEdge{Kind: kind, SrcID: src.ID, DstID: dst.ID, FilePath: src.FilePath, StartLine: line}
	}
	edges := []graphEdge{
		edge(graph.EdgeContains, spec, flow, 5), // doc → heading; must be ignored
		edge(graph.EdgeLinks, guide, flow, 3),   // guide links to spec.md#flow
		edge(graph.EdgeLinks, guide, spec, 4),   // guide links to spec.md (whole doc)
	}
	v := &graphView{
		nodesByID:   map[string]graphNode{},
		nodesByPath: map[string][]graphNode{},
		edgesBySrc:  map[string][]graphEdge{},
		edgesByDst:  map[string][]graphEdge{},
	}
	for _, n := range []graphNode{spec, guide, flow} {
		v.nodesByID[n.ID] = n
		v.nodesByPath[n.FilePath] = append(v.nodesByPath[n.FilePath], n)
	}
	for _, e := range edges {
		v.edgesBySrc[e.SrcID] = append(v.edgesBySrc[e.SrcID], e)
		v.edgesByDst[e.DstID] = append(v.edgesByDst[e.DstID], e)
	}

	// Backlinks of spec.md: both guide links roll up; the #flow one carries
	// the anchor. The contains edge is excluded.
	back := collectDocEdges(v, []graphNode{spec}, true, 50)
	if len(back) != 2 {
		t.Fatalf("backlinks = %d, want 2: %+v", len(back), back)
	}
	var sawAnchored, sawWhole bool
	for _, h := range back {
		if h.Kind == "contains" {
			t.Errorf("contains edge leaked into backlinks")
		}
		if h.Doc != "guide.md" {
			t.Errorf("backlink Doc = %q, want guide.md", h.Doc)
		}
		switch h.TargetAnchor {
		case "flow":
			sawAnchored = true
		case "":
			sawWhole = true
		}
	}
	if !sawAnchored || !sawWhole {
		t.Errorf("expected one #flow backlink and one whole-doc backlink; got %+v", back)
	}

	// Outgoing links of guide.md: the heading peer renders under its parent
	// doc (spec.md) with the anchor surfaced.
	out := collectDocEdges(v, []graphNode{guide}, false, 50)
	var sawHeadingPeer bool
	for _, h := range out {
		if h.Doc == "spec.md" && h.TargetAnchor == "flow" {
			sawHeadingPeer = true
		}
	}
	if !sawHeadingPeer {
		t.Errorf("outgoing links should render the #flow heading under spec.md; got %+v", out)
	}
}

// TestGraphTagsHelpers checks the tag-graph walks: a tag → its documents
// and a doc → its tags, ignoring non-`tagged` edges.
func TestGraphTagsHelpers(t *testing.T) {
	mkDoc := func(rel string) graphNode {
		return graphNode{ID: graph.NodeID("", "_markdown", graph.NodeDocument, rel), Kind: graph.NodeDocument, Name: baseName(rel), QualifiedName: rel, FilePath: rel}
	}
	mkTag := func(name string) graphNode {
		return graphNode{ID: graph.NodeID("", "_markdown", graph.NodeTag, name), Kind: graph.NodeTag, Name: name, QualifiedName: name}
	}
	a, b := mkDoc("a.md"), mkDoc("b.md")
	spec := mkTag("spec")
	edge := func(kind graph.EdgeKind, src, dst graphNode) graphEdge {
		return graphEdge{Kind: kind, SrcID: src.ID, DstID: dst.ID}
	}
	v := &graphView{
		nodesByID:  map[string]graphNode{},
		edgesBySrc: map[string][]graphEdge{},
		edgesByDst: map[string][]graphEdge{},
	}
	edges := []graphEdge{
		edge(graph.EdgeTagged, a, spec),
		edge(graph.EdgeTagged, b, spec),
		edge(graph.EdgeLinks, a, b), // not a tag edge — must be ignored
	}
	for _, n := range []graphNode{a, b, spec} {
		v.nodesByID[n.ID] = n
	}
	for _, e := range edges {
		v.edgesBySrc[e.SrcID] = append(v.edgesBySrc[e.SrcID], e)
		v.edgesByDst[e.DstID] = append(v.edgesByDst[e.DstID], e)
	}

	docs := docsForTag(v, spec.ID)
	if len(docs) != 2 {
		t.Errorf("docsForTag(spec) = %v, want [a.md b.md]", docs)
	}
	tags := tagsForDoc(v, a.ID)
	if len(tags) != 1 || tags[0] != "spec" {
		t.Errorf("tagsForDoc(a.md) = %v, want [spec] (links edge must be ignored)", tags)
	}
}

func TestResolveDocTargets(t *testing.T) {
	v := docVaultView()

	// Exact relpath.
	if got := resolveDocTargets(v, "specs/auth.md"); len(got) != 1 || got[0].QualifiedName != "specs/auth.md" {
		t.Errorf("exact path: got %+v", got)
	}
	// Root-level doc (QualifiedName == Name, so absent from nodesByQualified)
	// must still resolve via nodesByPath — regression guard for the
	// README.md not-found bug.
	if got := resolveDocTargets(v, "index.md"); len(got) != 1 || got[0].QualifiedName != "index.md" {
		t.Errorf("root-level doc: got %+v, want index.md", got)
	}
	// Leading ./ and extension completion.
	if got := resolveDocTargets(v, "./specs/auth"); len(got) != 1 || got[0].QualifiedName != "specs/auth.md" {
		t.Errorf("./path + ext completion: got %+v", got)
	}
	// Unique bare basename.
	if got := resolveDocTargets(v, "glossary"); len(got) != 1 || got[0].QualifiedName != "specs/glossary.md" {
		t.Errorf("bare basename: got %+v", got)
	}
	// Unknown path → no targets.
	if got := resolveDocTargets(v, "does/not/exist.md"); len(got) != 0 {
		t.Errorf("unknown path: got %+v, want none", got)
	}
}
