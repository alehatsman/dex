package lsprecall

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Cell is the per-repo recall summary.
type Cell struct {
	Repo       string        `json:"repo"`
	Lang       string        `json:"lang"`
	Probes     []ProbeResult `json:"probes"`
	MeanRecall float64       `json:"mean_recall"`
	LSPTotal   int           `json:"lsp_total"`
	GraphHits  int           `json:"graph_hits"`
	Errors     int           `json:"errors"`
}

// Suite is a collection of cells from one bench run.
type Suite struct {
	Cells []Cell `json:"cells"`
}

// NewSuite builds a Suite from raw probe results.
func NewSuite(cells []Cell) Suite { return Suite{Cells: cells} }

// LoadSuite deserializes a Suite from JSON (for --check baseline comparison).
func LoadSuite(data []byte) (Suite, error) {
	var s Suite
	if err := json.Unmarshal(data, &s); err != nil {
		return Suite{}, fmt.Errorf("lsprecall: parse suite: %w", err)
	}
	return s, nil
}

// JSON returns the suite serialized as indented JSON.
func (s Suite) JSON() ([]byte, error) {
	return json.MarshalIndent(s, "", "  ")
}

// Markdown renders a human-readable report table.
func (s Suite) Markdown() string {
	var b strings.Builder
	b.WriteString("## LSP vs graph recall\n\n")
	b.WriteString("| repo | lang | probes | lsp_total | graph_hits | recall | errors |\n")
	b.WriteString("|------|------|-------:|----------:|-----------:|-------:|-------:|\n")
	for _, c := range s.Cells {
		fmt.Fprintf(&b, "| %s | %s | %d | %d | %d | %.3f | %d |\n",
			c.Repo, c.Lang, len(c.Probes), c.LSPTotal, c.GraphHits, c.MeanRecall, c.Errors)
	}
	b.WriteString("\n### Per-probe detail\n\n")
	for _, c := range s.Cells {
		b.WriteString("#### " + c.Repo + "\n\n")
		b.WriteString("| symbol | direction | lsp | graph | recall | note |\n")
		b.WriteString("|--------|-----------|----:|------:|-------:|------|\n")
		for _, p := range c.Probes {
			note := ""
			if p.Error != "" {
				note = "err: " + p.Error
			} else if len(p.Missing) > 0 {
				note = "missing: " + strings.Join(p.Missing[:min(3, len(p.Missing))], ", ")
				if len(p.Missing) > 3 {
					note += fmt.Sprintf(" +%d", len(p.Missing)-3)
				}
			}
			fmt.Fprintf(&b, "| %s | %s | %d | %d | %.2f | %s |\n",
				p.Symbol, p.Direction, p.LSPCount, p.GraphHits, p.Recall(), note)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// BuildCell aggregates probe results into a Cell.
func BuildCell(repo, lang string, probes []ProbeResult) Cell {
	c := Cell{Repo: repo, Lang: lang, Probes: probes}
	var recallSum float64
	for _, p := range probes {
		c.LSPTotal += p.LSPCount
		c.GraphHits += p.GraphHits
		if p.Error != "" {
			c.Errors++
		} else {
			recallSum += p.Recall()
		}
	}
	n := len(probes) - c.Errors
	if n > 0 {
		c.MeanRecall = recallSum / float64(n)
	}
	return c
}

// DriftResult describes a recall regression vs a baseline.
type DriftResult struct {
	Repo   string
	Lang   string
	Before float64
	After  float64
	Delta  float64
}

func (d DriftResult) String() string {
	return fmt.Sprintf("%s/%s: recall %.3f → %.3f (Δ%.3f)", d.Repo, d.Lang, d.Before, d.After, d.Delta)
}

// Drift returns cells where mean recall dropped by more than tol vs ref.
func (s Suite) Drift(ref Suite, tol float64) []DriftResult {
	refMap := make(map[string]float64, len(ref.Cells))
	for _, c := range ref.Cells {
		refMap[c.Repo+"/"+c.Lang] = c.MeanRecall
	}
	var out []DriftResult
	for _, c := range s.Cells {
		key := c.Repo + "/" + c.Lang
		before, ok := refMap[key]
		if !ok {
			continue
		}
		delta := c.MeanRecall - before
		if delta < -tol {
			out = append(out, DriftResult{
				Repo:   c.Repo,
				Lang:   c.Lang,
				Before: before,
				After:  c.MeanRecall,
				Delta:  delta,
			})
		}
	}
	return out
}
