// Package codemap renders a deterministic, zero-inference orientation map of a
// repository (epic #316, story 1: `dex map`). It turns the call/import graph's
// Louvain communities into budgeted, multi-zoom text:
//
//   - L0 (~150 tokens): the repo at a glance — top clusters by aggregate
//     PageRank, each labeled by its dominant package with its highest-ranked
//     symbols. Answers "where does this codebase's weight live?"
//   - L1 (~1k tokens): one cluster in detail — members grouped by package, key
//     symbols with file:line. Answers "what's in this region and where?"
//
// The renderer is decoupled from the retrieval/graph stack: callers adapt their
// community data into Cluster/Symbol values, so rendering is pure and
// unit-testable with no index, store, or GPU. Budgets are enforced with
// internal/tokens so a map never blows an agent's context window.
package codemap

import (
	"fmt"
	"sort"
	"strings"

	"github.com/alehatsman/dex/internal/tokens"
)

// Symbol is one graph node placed in a cluster.
type Symbol struct {
	QualifiedName string
	Kind          string
	Pkg           string
	Path          string
	Line          int
	PageRank      float64
}

// Cluster is one Louvain community. Symbols are expected best-first
// (descending PageRank), as GraphCommunities returns them, and may be a
// PageRank-capped subset of the full membership — Size carries the true count.
type Cluster struct {
	ID      int
	Size    int // full community size; may exceed len(Symbols) when members are capped
	Symbols []Symbol
}

// size reports the cluster's member count for display, preferring the true
// community Size over the (possibly capped) number of fetched symbols.
func (c Cluster) size() int {
	if c.Size > len(c.Symbols) {
		return c.Size
	}
	return len(c.Symbols)
}

// Default token budgets per zoom level.
const (
	DefaultL0Budget = 150
	DefaultL1Budget = 1000
)

// weight is a cluster's aggregate importance: the sum of its members' PageRank.
func (c Cluster) weight() float64 {
	var w float64
	for _, s := range c.Symbols {
		w += s.PageRank
	}
	return w
}

// dominantPkg returns the package that carries the most PageRank within the
// cluster — the cluster's best one-word label. Ties break toward more members,
// then lexicographically, so the label is deterministic.
func dominantPkg(symbols []Symbol) string {
	type agg struct {
		weight float64
		count  int
	}
	byPkg := map[string]*agg{}
	for _, s := range symbols {
		pkg := s.Pkg
		if pkg == "" {
			pkg = "(unknown)"
		}
		a := byPkg[pkg]
		if a == nil {
			a = &agg{}
			byPkg[pkg] = a
		}
		a.weight += s.PageRank
		a.count++
	}
	best := ""
	var bw float64
	var bc int
	for pkg, a := range byPkg {
		if a.weight > bw ||
			(a.weight == bw && a.count > bc) ||
			(a.weight == bw && a.count == bc && (best == "" || pkg < best)) {
			best, bw, bc = pkg, a.weight, a.count
		}
	}
	return best
}

const l0Header = "# code map — top clusters by PageRank\n\n"

// ShownL0 returns the clusters RenderL0 would display under the token budget,
// in display order (aggregate PageRank, descending). It single-sources the
// greedy fit so callers can reason about exactly what an agent sees in the L0
// overview — e.g. the nav bench (story 7), which may only "zoom" a cluster the
// agent could actually have picked out of L0. At least one cluster is shown
// when any exist.
func ShownL0(clusters []Cluster, budget int) []Cluster {
	if budget <= 0 {
		budget = DefaultL0Budget
	}
	ranked := append([]Cluster(nil), clusters...)
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].weight() > ranked[j].weight() })
	if len(ranked) == 0 {
		return nil
	}

	// Budget against the grouped render, not per-cluster lines: a cluster
	// whose package is already shown only extends an existing headline, so
	// collapsing same-package clusters (#569) frees room for more distinct
	// packages instead of spending a whole slot per Louvain community.
	shown := make([]Cluster, 0, len(ranked))
	for _, c := range ranked {
		candidate := make([]Cluster, len(shown)+1)
		copy(candidate, shown)
		candidate[len(shown)] = c
		rendered := l0Header + strings.Join(groupShownByPkg(candidate), "")
		// Always show the first cluster; otherwise stop before exceeding budget.
		if len(shown) > 0 && tokens.Count(rendered) > budget {
			break
		}
		shown = candidate
	}
	return shown
}

// RenderL0 renders the repo-level map: clusters ranked by aggregate PageRank,
// one headline each (dominant package, size, top symbols), greedily fit to
// budget tokens. At least one cluster is always shown; a trailing note reports
// any clusters dropped to honor the budget.
func RenderL0(clusters []Cluster, budget int) string {
	if len(clusters) == 0 {
		return "# code map\n\n(no clusters — graph empty or not indexed)\n"
	}
	shown := ShownL0(clusters, budget)

	var b strings.Builder
	b.WriteString(l0Header)
	for _, line := range groupShownByPkg(shown) {
		b.WriteString(line)
	}
	if dropped := len(clusters) - len(shown); dropped > 0 {
		fmt.Fprintf(&b, "\n…%d more cluster(s). `dex map --cluster <id>` to zoom in.\n", dropped)
	}
	return b.String()
}

// l0RepNoise are low-signal identifiers that make poor cluster representatives
// — common receivers/locals/error vars that float up by PageRank but tell an
// agent nothing about what a cluster is (#569). Single- and two-char names are
// filtered separately by length.
var l0RepNoise = map[string]bool{
	"err": true, "ctx": true, "tmp": true, "val": true, "ret": true,
	"res": true, "out": true, "cur": true, "idx": true, "cnt": true,
	"buf": true, "msg": true, "req": true, "obj": true,
}

// topNames picks up to n representative symbol names, best-first by PageRank,
// skipping low-signal noise (l0RepNoise, ≤2-char names). If filtering leaves
// fewer than n, it backfills with the skipped names so a cluster of all-short
// names still shows something rather than nothing.
func topNames(symbols []Symbol, n int) []string {
	ranked := append([]Symbol(nil), symbols...)
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].PageRank > ranked[j].PageRank })

	tops := make([]string, 0, n)
	var skipped []string
	for _, s := range ranked {
		if len(tops) == n {
			break
		}
		name := shortName(s.QualifiedName)
		if len(name) <= 2 || l0RepNoise[strings.ToLower(name)] {
			skipped = append(skipped, name)
			continue
		}
		tops = append(tops, name)
	}
	for _, name := range skipped {
		if len(tops) == n {
			break
		}
		tops = append(tops, name)
	}
	return tops
}

// l0Line renders one cluster headline for L0 (the single-cluster case of
// l0Group); ShownL0 uses it to size each cluster against the budget.
func l0Line(c Cluster) string {
	return l0Group(dominantPkg(c.Symbols), []Cluster{c})
}

// l0Group renders one headline for a set of shown clusters that share a
// dominant package, so a package fragmented across several Louvain communities
// reads as one line (with every zoomable cluster id) instead of N near-identical
// rows that crowd out other packages (#569). The single-cluster form is
// byte-identical to the original l0Line output.
func l0Group(pkg string, group []Cluster) string {
	ids := make([]string, 0, len(group))
	total := 0
	syms := make([]Symbol, 0)
	for _, c := range group {
		ids = append(ids, fmt.Sprintf("#%d", c.ID))
		total += c.size()
		syms = append(syms, c.Symbols...)
	}
	return fmt.Sprintf("- %s **%s** (%d symbols): %s\n",
		strings.Join(ids, " "), pkg, total, strings.Join(topNames(syms, 3), ", "))
}

// groupShownByPkg merges the shown clusters that share a dominant package into
// one headline each, preserving weight order via first appearance.
func groupShownByPkg(shown []Cluster) []string {
	order := []string{}
	groups := map[string][]Cluster{}
	for _, c := range shown {
		pkg := dominantPkg(c.Symbols)
		if _, ok := groups[pkg]; !ok {
			order = append(order, pkg)
		}
		groups[pkg] = append(groups[pkg], c)
	}
	lines := make([]string, 0, len(order))
	for _, pkg := range order {
		lines = append(lines, l0Group(pkg, groups[pkg]))
	}
	return lines
}

// RenderL1 renders one cluster in detail: members grouped by package (packages
// ordered by aggregate PageRank), symbols listed best-first with kind and
// file:line, greedily fit to budget tokens.
func RenderL1(c Cluster, budget int) string {
	return renderGrouped(fmt.Sprintf("cluster #%d — %s", c.ID, dominantPkg(c.Symbols)), c.Symbols, c.size(), budget)
}

// RenderAround renders a task-focused region (issue #347, story 5): symbols
// assembled from a query's call-graph neighborhood (callers ∪ callees) or a
// diff's blast radius rather than from a Louvain community. Grouping is
// identical to RenderL1 so the view is familiar; the header carries the
// region's provenance (the seed symbol or the diff ref) instead of a cluster
// id, and size is the literal member count.
func RenderAround(title string, syms []Symbol, budget int) string {
	return renderGrouped(title, syms, len(syms), budget)
}

// renderGrouped renders a symbol set grouped by package — packages ordered by
// aggregate PageRank, symbols best-first within each — greedily fit to budget
// (DefaultL1Budget when budget <= 0). It backs both RenderL1 (a Louvain
// cluster) and RenderAround (a task-focused region); title is the
// "# <title> (<size> symbols)" header.
func renderGrouped(title string, symbols []Symbol, size, budget int) string {
	if budget <= 0 {
		budget = DefaultL1Budget
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s (%d symbols)\n\n", title, size)

	// Group symbols by package, preserving best-first order within each.
	order := []string{}
	byPkg := map[string][]Symbol{}
	for _, s := range symbols {
		pkg := s.Pkg
		if pkg == "" {
			pkg = "(unknown)"
		}
		if _, ok := byPkg[pkg]; !ok {
			order = append(order, pkg)
		}
		byPkg[pkg] = append(byPkg[pkg], s)
	}
	// Order packages by summed PageRank (descending).
	sort.SliceStable(order, func(i, j int) bool {
		return sumPageRank(byPkg[order[i]]) > sumPageRank(byPkg[order[j]])
	})

	truncated := false
	for _, pkg := range order {
		seg := l1Segment(pkg, byPkg[pkg])
		if b.Len() > 0 && tokens.Count(b.String()+seg) > budget {
			truncated = true
			break
		}
		b.WriteString(seg)
	}
	if truncated {
		b.WriteString("\n…more members omitted to fit budget.\n")
	}
	return b.String()
}

// RenderOrient composes the session-start orientation bundle: the L0 overview
// followed by an L1 zoom into the most-central cluster (the first ShownL0
// entry, ranked by aggregate PageRank). Deterministic and zero-inference — the
// single home both `ask("")` and `dex orient` render through, so they agree
// with `dex map` by construction (#348 / #316 story 6). Returns just L0 when no
// cluster is shown (empty or unindexed graph).
func RenderOrient(clusters []Cluster, l0budget, l1budget int) string {
	l0 := RenderL0(clusters, l0budget)
	shown := ShownL0(clusters, l0budget)
	if len(shown) == 0 {
		return l0
	}
	return l0 + "\n" + RenderL1(shown[0], l1budget)
}

// l1Segment renders one package block within a cluster.
func l1Segment(pkg string, symbols []Symbol) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## %s\n", pkg)
	for _, s := range symbols {
		loc := s.Path
		if s.Line > 0 {
			loc = fmt.Sprintf("%s:%d", s.Path, s.Line)
		}
		fmt.Fprintf(&b, "- %s (%s) — %s\n", shortName(s.QualifiedName), s.Kind, loc)
	}
	b.WriteString("\n")
	return b.String()
}

func sumPageRank(symbols []Symbol) float64 {
	var w float64
	for _, s := range symbols {
		w += s.PageRank
	}
	return w
}

// shortName trims a package-qualified name to its last segment for compact
// display (e.g. "github.com/x/y.Foo" -> "Foo", "pkg.Bar" -> "Bar"), but keeps
// already-short names intact.
func shortName(qn string) string {
	if i := strings.LastIndexAny(qn, "./"); i >= 0 && i < len(qn)-1 {
		return qn[i+1:]
	}
	return qn
}
