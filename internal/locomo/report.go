package locomo

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// CategoryStats aggregates scores for one question category.
type CategoryStats struct {
	Category        string  `json:"category"`
	N               int     `json:"n"`
	RecallAtK       float64 `json:"recall_at_k"`
	MeanTokenF1     float64 `json:"mean_token_f1"`
	ExactMatchRate  float64 `json:"exact_match_rate"`
	AvgTokenSavings float64 `json:"avg_token_savings"`
}

// Report is the full benchmark output.
type Report struct {
	K          int             `json:"k"`
	Overall    CategoryStats   `json:"overall"`
	Categories []CategoryStats `json:"categories"`
}

// Compute builds a Report from a set of QuestionResults.
func Compute(results []QuestionResult, k int) Report {
	catMap := make(map[string]*categoryAcc)
	overall := &categoryAcc{category: "overall"}

	for _, r := range results {
		acc := catMap[r.Category]
		if acc == nil {
			acc = &categoryAcc{category: r.Category}
			catMap[r.Category] = acc
		}
		acc.add(r)
		overall.add(r)
	}

	cats := make([]CategoryStats, 0, len(catMap))
	for _, acc := range catMap {
		cats = append(cats, acc.stats())
	}
	sort.Slice(cats, func(i, j int) bool { return cats[i].Category < cats[j].Category })

	return Report{
		K:          k,
		Overall:    overall.stats(),
		Categories: cats,
	}
}

// JSON serializes the report as indented JSON.
func (rep Report) JSON() ([]byte, error) {
	return json.MarshalIndent(rep, "", "  ")
}

// Markdown renders the report as a GitHub-flavored Markdown table.
func (rep Report) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "## LoCoMo Benchmark — k=%d\n\n", rep.K)
	b.WriteString("| Category | N | Recall@k | Mean F1 | Exact Match | Token Savings |\n")
	b.WriteString("|----------|---|----------|---------|-------------|---------------|\n")
	writeRow(&b, rep.Overall)
	for _, c := range rep.Categories {
		writeRow(&b, c)
	}
	return b.String()
}

func writeRow(b *strings.Builder, s CategoryStats) {
	savings := ""
	if s.AvgTokenSavings > 0 {
		savings = fmt.Sprintf("%.1f%%", s.AvgTokenSavings*100)
	} else {
		savings = "—"
	}
	fmt.Fprintf(b, "| %-12s | %3d | %7.3f | %7.3f | %11.3f | %13s |\n",
		s.Category, s.N, s.RecallAtK, s.MeanTokenF1, s.ExactMatchRate, savings)
}

type categoryAcc struct {
	category     string
	n            int
	recallHits   int
	f1Sum        float64
	exactHits    int
	savingsSum   float64
	savingsCount int
}

func (a *categoryAcc) add(r QuestionResult) {
	a.n++
	if r.RecallAtK {
		a.recallHits++
	}
	a.f1Sum += r.BestTokenF1
	if r.AnyExactMatch {
		a.exactHits++
	}
	if r.TranscriptTokens > 0 && r.RetrievedTokens > 0 {
		saved := 1.0 - float64(r.RetrievedTokens)/float64(r.TranscriptTokens)
		if saved > 0 {
			a.savingsSum += saved
			a.savingsCount++
		}
	}
}

func (a *categoryAcc) stats() CategoryStats {
	if a.n == 0 {
		return CategoryStats{Category: a.category}
	}
	avgSavings := 0.0
	if a.savingsCount > 0 {
		avgSavings = a.savingsSum / float64(a.savingsCount)
	}
	return CategoryStats{
		Category:        a.category,
		N:               a.n,
		RecallAtK:       float64(a.recallHits) / float64(a.n),
		MeanTokenF1:     a.f1Sum / float64(a.n),
		ExactMatchRate:  float64(a.exactHits) / float64(a.n),
		AvgTokenSavings: avgSavings,
	}
}
