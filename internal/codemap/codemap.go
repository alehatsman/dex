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

// RenderL0 renders the repo-level map: clusters ranked by aggregate PageRank,
// one headline each (dominant package, size, top symbols), greedily fit to
// budget tokens. At least one cluster is always shown; a trailing note reports
// any clusters dropped to honor the budget.
func RenderL0(clusters []Cluster, budget int) string {
	if budget <= 0 {
		budget = DefaultL0Budget
	}
	ranked := append([]Cluster(nil), clusters...)
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].weight() > ranked[j].weight() })

	if len(ranked) == 0 {
		return "# code map\n\n(no clusters — graph empty or not indexed)\n"
	}

	const header = "# code map — top clusters by PageRank\n\n"
	var b strings.Builder
	b.WriteString(header)

	shown := 0
	for _, c := range ranked {
		line := l0Line(c)
		// Always show the first cluster; otherwise stop before exceeding budget.
		if shown > 0 && tokens.Count(b.String()+line) > budget {
			break
		}
		b.WriteString(line)
		shown++
	}
	if dropped := len(ranked) - shown; dropped > 0 {
		fmt.Fprintf(&b, "\n…%d more cluster(s). `dex map --cluster <id>` to zoom in.\n", dropped)
	}
	return b.String()
}

// l0Line renders one cluster headline for L0.
func l0Line(c Cluster) string {
	pkg := dominantPkg(c.Symbols)
	tops := make([]string, 0, 3)
	for _, s := range c.Symbols {
		if len(tops) == 3 {
			break
		}
		tops = append(tops, shortName(s.QualifiedName))
	}
	return fmt.Sprintf("- #%d **%s** (%d symbols): %s\n", c.ID, pkg, c.size(), strings.Join(tops, ", "))
}

// RenderL1 renders one cluster in detail: members grouped by package (packages
// ordered by aggregate PageRank), symbols listed best-first with kind and
// file:line, greedily fit to budget tokens.
func RenderL1(c Cluster, budget int) string {
	if budget <= 0 {
		budget = DefaultL1Budget
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# cluster #%d — %s (%d symbols)\n\n", c.ID, dominantPkg(c.Symbols), c.size())

	// Group symbols by package, preserving best-first order within each.
	order := []string{}
	byPkg := map[string][]Symbol{}
	for _, s := range c.Symbols {
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
