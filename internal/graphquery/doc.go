package graphquery

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/alehatsman/dex/internal/graph"
)

// DocEdge is one resolved doc-link edge: the peer document, the link kind, and
// the file:line of the link expression that produced it. Fields mirror the mcp
// wire type so callers convert directly.
type DocEdge struct {
	Doc          string
	Name         string
	Kind         string
	LinkSitePath string
	LinkSiteLine int
	TargetAnchor string
}

// DocsForTag returns the distinct documents carrying the tag node, via
// its incoming `tagged` edges. Pure over view — unit-testable.
func DocsForTag(view *View, tagID string) []string {
	seen := map[string]bool{}
	var docs []string
	for _, e := range view.EdgesByDst[tagID] {
		if e.Kind != graph.EdgeTagged {
			continue
		}
		if src, ok := view.NodesByID[e.SrcID]; ok && !seen[src.QualifiedName] {
			seen[src.QualifiedName] = true
			docs = append(docs, src.QualifiedName)
		}
	}
	return docs
}

// TagsForDoc returns the tag names a document carries, via its outgoing
// `tagged` edges. Pure over view — unit-testable.
func TagsForDoc(view *View, docID string) []string {
	var tags []string
	for _, e := range view.EdgesBySrc[docID] {
		if e.Kind != graph.EdgeTagged {
			continue
		}
		if dst, ok := view.NodesByID[e.DstID]; ok {
			tags = append(tags, dst.Name)
		}
	}
	return tags
}

// SortByDocCentrality orders doc relpaths by their node's PageRank then
// in-degree (most-referenced first), with path as the deterministic
// tiebreaker. Docs absent from the view sort last.
func SortByDocCentrality(view *View, docs []string) {
	rank := func(rel string) (float64, int) {
		for _, n := range view.NodesByPath[rel] {
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

// ResolveDocTargets maps the input `doc` onto document nodes. It tries an
// exact relpath match first (the qualified name of a doc node is its
// relpath), then falls back to a unique basename match for convenience.
func ResolveDocTargets(view *View, doc string) []Node {
	doc = strings.TrimSpace(doc)
	doc = strings.TrimPrefix(filepath.ToSlash(doc), "./")
	if doc == "" {
		return nil
	}
	isDoc := func(n Node) bool { return n.Kind == graph.NodeDocument }

	// 1) Exact relpath match, with/without an extension. Resolve via
	//    NodesByPath (keyed on FilePath) — NOT NodesByQualified, which
	//    Load populates only when QualifiedName != Name, so a
	//    root-level doc like "README.md" (where they're equal) is absent
	//    from it. A document's FilePath is its relpath, so this is also
	//    the natural index for a path lookup.
	for _, cand := range []string{doc, doc + ".md", doc + ".markdown"} {
		for _, n := range view.NodesByPath[cand] {
			if isDoc(n) {
				return []Node{n}
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
	var matches []Node
	for _, n := range view.DocNodes() {
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

// CollectDocEdges walks the doc-graph edges incident to targets and
// returns the peer documents, keeping only `links`/`wikilinks`/
// `transcludes` edges so code edges (`calls`/`imports`/…) never leak in.
// backlinks=false walks outgoing edges (docs the target points to);
// backlinks=true walks incoming edges (docs that point to the target),
// rolled up across the doc's heading nodes so a link to `doc.md#section`
// still counts as a backlink of `doc.md`. When an edge targets a heading,
// TargetAnchor names the section and Doc resolves to the parent document.
// Hits are deduped, sorted deterministically, and capped at k. Pure over
// view — unit-testable off a hand-built graph.
func CollectDocEdges(view *View, targets []Node, backlinks bool, k int) []DocEdge {
	seen := map[string]bool{}
	hits := []DocEdge{}
	for _, t := range targets {
		// For backlinks, scan edges incident to the doc AND each of its
		// heading nodes (same FilePath, kind heading), so section-targeted
		// links roll up to the document. Outgoing links always originate
		// from the document node, so no expansion is needed there.
		endpoints := []string{t.ID}
		if backlinks {
			for _, n := range view.NodesByPath[t.FilePath] {
				if n.Kind == graph.NodeHeading {
					endpoints = append(endpoints, n.ID)
				}
			}
		}
		for _, id := range endpoints {
			edges := view.EdgesBySrc[id]
			if backlinks {
				edges = view.EdgesByDst[id]
			}
			for _, e := range edges {
				if e.Kind != graph.EdgeLinks && e.Kind != graph.EdgeWikilinks && e.Kind != graph.EdgeTransclude {
					continue
				}
				peerID := e.DstID
				if backlinks {
					peerID = e.SrcID
				}
				peer, ok := view.NodesByID[peerID]
				if !ok {
					continue
				}
				// Render the peer: a heading peer surfaces under its parent
				// doc with its text as the name.
				doc, name := peer.QualifiedName, peer.Name
				if peer.Kind == graph.NodeHeading {
					doc = peer.FilePath
				}
				// The link's destination names the section, if any — true in
				// both directions since edges always point src → (doc|heading).
				var anchor string
				if dn, ok := view.NodesByID[e.DstID]; ok && dn.Kind == graph.NodeHeading {
					anchor = anchorOf(dn.QualifiedName)
				}
				key := peerID + "|" + string(e.Kind) + "|" + e.FilePath + ":" + fmt.Sprint(e.StartLine) + "|" + anchor
				if seen[key] {
					continue
				}
				seen[key] = true
				hits = append(hits, DocEdge{
					Doc:          doc,
					Name:         name,
					Kind:         string(e.Kind),
					LinkSitePath: e.FilePath,
					LinkSiteLine: e.StartLine,
					TargetAnchor: anchor,
				})
			}
		}
	}
	// Order by peer importance (doc-graph PageRank, then backlink in-degree)
	// so the most-referenced docs surface first, with path/kind/line/anchor
	// as deterministic tiebreakers. peerRank reads the centrality persisted
	// on the peer's document node; a heading peer borrows its parent doc's
	// rank.
	peerRank := func(h DocEdge) (float64, int) {
		for _, n := range view.NodesByPath[h.Doc] {
			if n.Kind == graph.NodeDocument {
				return n.PageRank, n.InDegree
			}
		}
		return 0, 0
	}
	sort.SliceStable(hits, func(i, j int) bool {
		a, b := hits[i], hits[j]
		pa, da := peerRank(a)
		pb, db := peerRank(b)
		if pa != pb {
			return pa > pb
		}
		if da != db {
			return da > db
		}
		if a.Doc != b.Doc {
			return a.Doc < b.Doc
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.LinkSiteLine != b.LinkSiteLine {
			return a.LinkSiteLine < b.LinkSiteLine
		}
		return a.TargetAnchor < b.TargetAnchor
	})
	if len(hits) > k {
		hits = hits[:k]
	}
	return hits
}

// anchorOf returns the slug part of a heading QualifiedName ("doc.md#sec"
// → "sec"); "" when there's no fragment.
func anchorOf(qualifiedName string) string {
	if i := strings.Index(qualifiedName, "#"); i >= 0 {
		return qualifiedName[i+1:]
	}
	return ""
}
