// Package skew quantifies cross-language centrality skew over a loaded
// call graph — gate-2 of epic #468.
//
// The skew is an artifact of resolution accuracy, not real importance
// (docs/architecture.md): Go `calls` edges are type-resolved via go/types,
// so method/receiver dispatch binds exactly and Go function nodes accrue a
// dense in-edge neighbourhood. The tree-sitter languages are name-resolved
// with no type info — same-file and import-table calls resolve, but calls on
// typed receivers (`x.method()`) are frequently dropped, leaving a sparser
// graph. PageRank over that mixed graph therefore concentrates on Go nodes
// out of proportion to how many nodes each language actually contributes.
//
// This package makes that distortion observable. For the call-graph
// participants (function + method nodes) it groups by language and reports,
// per language, the node-count share against the PageRank-mass share; their
// ratio is the skew. A language with skew_ratio > 1 holds more centrality
// mass than its node count warrants; < 1 means it is under-weighted. In a
// healthy (resolution-parity) graph every language sits near 1.
//
// Measurement only — it reads the persisted centrality the graph tools
// actually serve, so the number reflects the artifact users see, not a
// recomputation. No resolver changes live here.
package skew

import (
	"encoding/json"
	"sort"

	"github.com/alehatsman/dex/internal/graph"
	"github.com/alehatsman/dex/internal/graphquery"
)

// LangStat is the per-language skew breakdown over call-graph participants.
type LangStat struct {
	Language      string  `json:"language"`
	NodeCount     int     `json:"node_count"`
	NodeShare     float64 `json:"node_share"`     // node_count / total nodes
	PageRankMass  float64 `json:"pagerank_mass"`  // Σ pagerank over this language
	PageRankShare float64 `json:"pagerank_share"` // pagerank_mass / total pagerank
	SkewRatio     float64 `json:"skew_ratio"`     // pagerank_share / node_share; >1 over-weighted
	Communities   int     `json:"communities"`    // distinct Louvain community IDs (>0) the language occupies
}

// Report is the whole-graph skew summary. Languages are sorted by
// descending PageRankShare (ties broken by language name) for stable output.
type Report struct {
	TotalNodes       int        `json:"total_nodes"`       // function + method nodes
	TotalPageRank    float64    `json:"total_pagerank"`    // Σ pagerank over those nodes
	TotalCommunities int        `json:"total_communities"` // distinct community IDs (>0) graph-wide
	Languages        []LangStat `json:"languages"`
}

// Compute derives the skew report from a loaded view. Pure (no I/O) so it
// unit-tests against a hand-built view. A nil/empty view yields a zero Report.
//
// Population = NodeFunction + NodeMethod, the only kinds the call graph
// (and therefore PageRank) touches; types/fields/packages carry zero
// centrality by design and would dilute the shares, so they are excluded.
func Compute(view *graphquery.View) Report {
	if view == nil {
		return Report{}
	}

	type acc struct {
		count int
		pr    float64
		comms map[int]struct{}
	}
	byLang := map[string]*acc{}
	allComms := map[int]struct{}{}
	var totalNodes int
	var totalPR float64

	for _, n := range view.NodesByID {
		if n.Kind != graph.NodeFunction && n.Kind != graph.NodeMethod {
			continue
		}
		lang := nodeLanguage(n)
		a := byLang[lang]
		if a == nil {
			a = &acc{comms: map[int]struct{}{}}
			byLang[lang] = a
		}
		a.count++
		a.pr += n.PageRank
		totalNodes++
		totalPR += n.PageRank
		if n.CommunityID > 0 {
			a.comms[n.CommunityID] = struct{}{}
			allComms[n.CommunityID] = struct{}{}
		}
	}

	rep := Report{
		TotalNodes:       totalNodes,
		TotalPageRank:    totalPR,
		TotalCommunities: len(allComms),
		Languages:        make([]LangStat, 0, len(byLang)),
	}
	for lang, a := range byLang {
		st := LangStat{
			Language:     lang,
			NodeCount:    a.count,
			PageRankMass: a.pr,
			Communities:  len(a.comms),
		}
		if totalNodes > 0 {
			st.NodeShare = float64(a.count) / float64(totalNodes)
		}
		if totalPR > 0 {
			st.PageRankShare = a.pr / totalPR
		}
		if st.NodeShare > 0 {
			st.SkewRatio = st.PageRankShare / st.NodeShare
		}
		rep.Languages = append(rep.Languages, st)
	}

	sort.Slice(rep.Languages, func(i, j int) bool {
		a, b := rep.Languages[i], rep.Languages[j]
		if a.PageRankShare != b.PageRankShare {
			return a.PageRankShare > b.PageRankShare
		}
		return a.Language < b.Language
	})
	return rep
}

// nodeLanguage reports a node's source language using the same convention as
// the package-graph tool: tree-sitter extractors stamp Metadata["language"];
// the Go extractor leaves it absent. So no metadata / no "language" key → Go.
func nodeLanguage(n graphquery.Node) string {
	if len(n.MetadataJSON) == 0 {
		return "go"
	}
	var md map[string]any
	if err := json.Unmarshal(n.MetadataJSON, &md); err != nil {
		return "go"
	}
	if lang, ok := md["language"].(string); ok && lang != "" {
		return lang
	}
	return "go"
}
