package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/alehatsman/dex/internal/graph"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ─── tool: graph_tags ─────────────────────────────────────────────────────

type TagInput struct {
	Tag         string `json:"tag,omitempty" jsonschema:"a markdown #tag (without the leading #) — returns the documents carrying it"`
	Doc         string `json:"doc,omitempty" jsonschema:"a document path — returns the tags that document carries; ignored when tag is set"`
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
	K           int    `json:"k,omitempty" jsonschema:"max items to return (default 100, max 500)"`
}

type TagOutput struct {
	Status  string   `json:"status"`            // "ok" | "no-index" | "no-graph" | "not-found" | "error"
	Hint    string   `json:"hint,omitempty"`    //
	Project string   `json:"project,omitempty"` //
	Query   string   `json:"query,omitempty"`   // the resolved tag or doc
	Result  string   `json:"result,omitempty"`  // "documents" (tag→docs) or "tags" (doc→tags)
	Items   []string `json:"items,omitempty"`   // doc relpaths, or tag names
}

func (s *Server) GraphTags(ctx context.Context, in TagInput) (TagOutput, error) {
	_, out, err := s.graphTags(ctx, nil, in)
	return out, err
}

// graphTags answers the two tag-clustering questions over the doc graph's
// `tagged` edges: `tag` → the documents carrying it; `doc` → the tags it
// carries. Exactly one of tag/doc is used (tag wins if both are set).
func (s *Server) graphTags(ctx context.Context, _ *sdk.CallToolRequest, in TagInput) (*sdk.CallToolResult, TagOutput, error) {
	tag := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(in.Tag), "#"))
	doc := strings.TrimSpace(in.Doc)
	if tag == "" && doc == "" {
		return nil, TagOutput{Status: "error", Hint: "pass `tag` (→ documents) or `doc` (→ tags)"}, nil
	}
	p, hint := s.resolveProject(in.ProjectRoot)
	if hint != "" {
		return nil, TagOutput{Status: "error", Hint: hint}, nil
	}
	if _, err := os.Stat(p.DBPath); errors.Is(err, os.ErrNotExist) {
		return nil, TagOutput{Status: "no-index", Project: p.Root,
			Hint: fmt.Sprintf("no index for %s — run `dex index %s` first.", p.Root, p.Root)}, nil
	}
	st, err := s.openStore(p.DBPath)
	if err != nil {
		return nil, TagOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
	}

	view, err := s.cachedLoadGraphView(ctx, st, p.DBPath)
	if err != nil {
		return nil, TagOutput{Status: "error", Hint: fmt.Sprintf("load graph: %v", err)}, nil
	}
	if view == nil {
		return nil, TagOutput{Status: "no-graph", Project: p.Root,
			Hint: fmt.Sprintf("graph not indexed for %s — run `dex index %s --graph=only`.", p.Root, p.Root)}, nil
	}

	k := in.K
	if k <= 0 {
		k = 100
	}
	if k > 500 {
		k = 500
	}

	if tag != "" {
		// tag → documents. Find the tag node by name, walk its incoming
		// `tagged` edges, collect the source documents.
		var tagNode *graphNode
		for _, n := range view.nodesByName[tag] {
			if n.Kind == graph.NodeTag {
				nn := n
				tagNode = &nn
				break
			}
		}
		if tagNode == nil {
			return nil, TagOutput{Status: "not-found", Project: p.Root, Query: tag,
				Hint: fmt.Sprintf("no #%s tag in the doc graph.", tag)}, nil
		}
		docs := docsForTag(view, tagNode.ID)
		sortByDocCentrality(view, docs)
		if len(docs) > k {
			docs = docs[:k]
		}
		return nil, TagOutput{Status: "ok", Project: p.Root, Query: tag, Result: "documents", Items: docs}, nil
	}

	// doc → tags.
	targets := resolveDocTargets(view, doc)
	if len(targets) == 0 {
		return nil, TagOutput{Status: "not-found", Project: p.Root, Query: doc,
			Hint: fmt.Sprintf("no document node matches %q.", doc)}, nil
	}
	seen := map[string]bool{}
	var tags []string
	for _, t := range targets {
		for _, name := range tagsForDoc(view, t.ID) {
			if !seen[name] {
				seen[name] = true
				tags = append(tags, name)
			}
		}
	}
	sort.Strings(tags)
	if len(tags) > k {
		tags = tags[:k]
	}
	return nil, TagOutput{Status: "ok", Project: p.Root, Query: targets[0].QualifiedName, Result: "tags", Items: tags}, nil
}

// docsForTag returns the distinct documents carrying the tag node, via
// its incoming `tagged` edges. Pure over view — unit-testable.
func docsForTag(view *graphView, tagID string) []string {
	seen := map[string]bool{}
	var docs []string
	for _, e := range view.edgesByDst[tagID] {
		if e.Kind != graph.EdgeTagged {
			continue
		}
		if src, ok := view.nodesByID[e.SrcID]; ok && !seen[src.QualifiedName] {
			seen[src.QualifiedName] = true
			docs = append(docs, src.QualifiedName)
		}
	}
	return docs
}

// tagsForDoc returns the tag names a document carries, via its outgoing
// `tagged` edges. Pure over view — unit-testable.
func tagsForDoc(view *graphView, docID string) []string {
	var tags []string
	for _, e := range view.edgesBySrc[docID] {
		if e.Kind != graph.EdgeTagged {
			continue
		}
		if dst, ok := view.nodesByID[e.DstID]; ok {
			tags = append(tags, dst.Name)
		}
	}
	return tags
}

// sortByDocCentrality orders doc relpaths by their node's PageRank then
// in-degree (most-referenced first), with path as the deterministic
// tiebreaker. Docs absent from the view sort last.
func sortByDocCentrality(view *graphView, docs []string) {
	rank := func(rel string) (float64, int) {
		for _, n := range view.nodesByPath[rel] {
			if n.Kind == graph.NodeDocument {
				return n.PageRank, n.InDegree
			}
		}
		return 0, 0
	}
	sort.SliceStable(docs, func(i, j int) bool {
		pi, di := rank(docs[i])
		pj, dj := rank(docs[j])
		if pi != pj {
			return pi > pj
		}
		if di != dj {
			return di > dj
		}
		return docs[i] < docs[j]
	})
}

// resolveDocTargets maps the input `doc` onto document nodes. It tries an
// exact relpath match first (the qualified name of a doc node is its
// relpath), then falls back to a unique basename match for convenience.
func resolveDocTargets(view *graphView, doc string) []graphNode {
	doc = strings.TrimSpace(doc)
	doc = strings.TrimPrefix(filepath.ToSlash(doc), "./")
	if doc == "" {
		return nil
	}
	isDoc := func(n graphNode) bool { return n.Kind == graph.NodeDocument }

	// 1) Exact relpath match, with/without an extension. Resolve via
	//    nodesByPath (keyed on FilePath) — NOT nodesByQualified, which
	//    loadGraphView populates only when QualifiedName != Name, so a
	//    root-level doc like "README.md" (where they're equal) is absent
	//    from it. A document's FilePath is its relpath, so this is also
	//    the natural index for a path lookup.
	for _, cand := range []string{doc, doc + ".md", doc + ".markdown"} {
		for _, n := range view.nodesByPath[cand] {
			if isDoc(n) {
				return []graphNode{n}
			}
		}
	}

	// 2) Unique basename match across document nodes.
	base := strings.ToLower(doc)
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	if dot := strings.LastIndex(base, "."); dot >= 0 {
		base = base[:dot]
	}
	var matches []graphNode
	for _, n := range view.docNodes() {
		nb := strings.ToLower(n.Name)
		if dot := strings.LastIndex(nb, "."); dot >= 0 {
			nb = nb[:dot]
		}
		if nb == base {
			matches = append(matches, n)
		}
	}
	if len(matches) == 1 {
		return matches
	}
	// Ambiguous (or zero) basename → caller reports not-found; exact
	// relpath is the disambiguator.
	return nil
}
