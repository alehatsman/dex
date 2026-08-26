package mcp

import "strconv"

// query_refs.go surfaces the uniform Selection currency on the query envelope
// (#207 / docs/design/95f-selection-spine.md). Every lane, whatever its rich
// typed payload, also yields a flat list of located entities — Refs — so an
// agent (and, in #206, a pipe stage) has one shape to thread regardless of which
// lane ran. The typed payload stays authoritative for rendering; Refs is a
// lightweight index OVER it, never a replacement (embed-not-subsume, #95f §3).

// Ref is one located code entity on the wire — the L4 twin of retrieve.Ref. It
// is a locator, not a payload: the lane's typed result carries the bytes.
type Ref struct {
	Kind  string  `json:"kind"`            // file | symbol | chunk | package | edge
	ID    string  `json:"id"`              // stable id: path, path:line, qualified symbol
	Path  string  `json:"path,omitempty"`  // rel path when applicable
	Span  []int   `json:"span,omitempty"`  // [startLine, endLine] when applicable
	Prov  string  `json:"prov,omitempty"`  // exact | semantic | name-based
	Score float64 `json:"score,omitempty"` // lane-relative rank when applicable
}

// span builds the omitempty-friendly [start,end] locator, nil when unknown so it
// disappears from the wire rather than serializing [0,0].
func span(start, end int) []int {
	if start <= 0 {
		return nil
	}
	if end < start {
		end = start
	}
	return []int{start, end}
}

// refsFromExact flattens the populated exact lane of a QueryResult into the
// uniform Ref index. Exactly one of the four is set (route.lane names it);
// provenance is "exact" for every deterministic lane except a partial-recall
// trace, which is honestly "name-based".
func refsFromExact(r QueryResult) []Ref {
	switch {
	case r.Read != nil:
		return refsFromRead(r.Read)
	case r.Grep != nil:
		return refsFromGrep(r.Grep)
	case r.Trace != nil:
		return refsFromTrace(r.Trace)
	case r.Locate != nil:
		return refsFromLocate(r.Locate)
	}
	return nil
}

func refsFromRead(o *SummarizeOutput) []Ref {
	if len(o.Paths) > 0 { // batch read
		refs := make([]Ref, 0, len(o.Paths))
		for _, p := range o.Paths {
			refs = append(refs, Ref{Kind: "file", ID: p, Path: p, Prov: "exact"})
		}
		return refs
	}
	if o.Path == "" {
		return nil
	}
	return []Ref{{Kind: "file", ID: o.Path, Path: o.Path, Span: span(o.StartLine, o.EndLine), Prov: "exact"}}
}

func refsFromGrep(o *SearchGrepOutput) []Ref {
	refs := make([]Ref, 0, len(o.Matches))
	for _, m := range o.Matches {
		refs = append(refs, Ref{
			Kind: "chunk",
			ID:   m.Path + ":" + strconv.Itoa(m.Line),
			Path: m.Path,
			Span: span(m.Line, m.Line),
			Prov: "exact",
		})
	}
	return refs
}

func refsFromTrace(o *TraceOutput) []Ref {
	prov := "exact"
	if o.Recall == "partial" {
		prov = "name-based"
	}
	refs := make([]Ref, 0, len(o.Hits)+len(o.Nodes))
	for _, h := range o.Hits {
		refs = append(refs, Ref{
			Kind: "symbol", ID: h.QualifiedName, Path: h.Path,
			Span: span(h.StartLine, h.EndLine), Prov: prov,
		})
	}
	for _, n := range o.Nodes {
		refs = append(refs, Ref{
			Kind: "symbol", ID: n.QualifiedName, Path: n.Path,
			Span: span(n.StartLine, n.StartLine), Prov: prov,
		})
	}
	return refs
}

func refsFromLocate(o *LocateOutput) []Ref {
	if o.Path == "" && o.Symbol == "" {
		return nil
	}
	kind := o.Kind
	if kind == "" {
		kind = "symbol"
	}
	id := o.Symbol
	if id == "" {
		id = o.Path
	}
	return []Ref{{Kind: kind, ID: id, Path: o.Path, Span: span(o.StartLine, o.EndLine), Prov: "exact"}}
}

// refsFromSemantic flattens the semantic evidence (symbols + hits + suggested
// reads) into the uniform Ref index. Symbols are name-based (tree-sitter match);
// hits and reads are semantic (embedding retrieval). Orient/review carry no
// per-entity evidence, so they yield no refs.
func refsFromSemantic(co *ContextOutput) []Ref {
	refs := make([]Ref, 0, len(co.Symbols)+len(co.SemanticHits)+len(co.SuggestedReads))
	for _, s := range co.Symbols {
		refs = append(refs, Ref{
			Kind: "symbol", ID: s.QualifiedName, Path: s.Path,
			Span: span(s.StartLine, s.EndLine), Prov: "name-based",
		})
	}
	for _, h := range co.SemanticHits {
		refs = append(refs, Ref{
			Kind: "chunk", ID: h.Path + ":" + strconv.Itoa(h.StartLine), Path: h.Path,
			Span: span(h.StartLine, h.EndLine), Prov: "semantic", Score: float64(h.Score),
		})
	}
	for _, sr := range co.SuggestedReads {
		refs = append(refs, Ref{
			Kind: "file", ID: sr.Path, Path: sr.Path,
			Span: span(sr.StartLine, sr.EndLine), Prov: "semantic",
		})
	}
	if len(refs) == 0 {
		return nil
	}
	return refs
}
