package corpus

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// CorpusReport aggregates the per-(repo, set) cells of a corpus run. It carries
// the raw cells; per-language and grand-mean rollups are derived on render so
// the JSON baseline stays a flat, diffable list of cells.
type CorpusReport struct {
	K     int             `json:"k"`
	Cells []LabeledReport `json:"cells"`
}

// Compute wraps the scored cells into a CorpusReport.
func Compute(cells []LabeledReport, k int) CorpusReport {
	return CorpusReport{K: k, Cells: cells}
}

type meanRow struct {
	label             string
	n                 int // number of cells averaged
	ndcg, recall, mrr float64
}

func mean(cells []LabeledReport) meanRow {
	var r meanRow
	if len(cells) == 0 {
		return r
	}
	for _, c := range cells {
		r.ndcg += c.Report.MeanNDCG
		r.recall += c.Report.MeanRecall
		r.mrr += c.Report.MRR
	}
	n := float64(len(cells))
	r.n = len(cells)
	r.ndcg /= n
	r.recall /= n
	r.mrr /= n
	return r
}

// byLanguage groups cells by primary language, returning rows sorted by label.
// Each language's metrics are the unweighted mean of its cells (one vote per
// (repo, set) cell) — curated and generated sets count equally.
func (r CorpusReport) byLanguage() []meanRow {
	groups := map[string][]LabeledReport{}
	for _, c := range r.Cells {
		groups[c.Lang] = append(groups[c.Lang], c)
	}
	var rows []meanRow
	for lang, cells := range groups {
		m := mean(cells)
		m.label = lang
		rows = append(rows, m)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].label < rows[j].label })
	return rows
}

// Markdown renders the per-cell table, the per-language rollup and the grand
// mean.
func (r CorpusReport) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Corpus Retrieval Eval — k=%d, %d cells\n\n", r.K, len(r.Cells))

	b.WriteString("| repo | lang | set | n | NDCG | Recall | MRR |\n")
	b.WriteString("|------|------|-----|---|------|--------|-----|\n")
	cells := append([]LabeledReport(nil), r.Cells...)
	sort.Slice(cells, func(i, j int) bool {
		if cells[i].Repo != cells[j].Repo {
			return cells[i].Repo < cells[j].Repo
		}
		return cells[i].Set < cells[j].Set
	})
	for _, c := range cells {
		fmt.Fprintf(&b, "| %s | %s | %s | %d | %.3f | %.3f | %.3f |\n",
			c.Repo, c.Lang, c.Set, c.Report.N, c.Report.MeanNDCG, c.Report.MeanRecall, c.Report.MRR)
	}

	b.WriteString("\n### Per-language (unweighted mean of cells)\n\n")
	b.WriteString("| lang | cells | NDCG | Recall | MRR |\n")
	b.WriteString("|------|-------|------|--------|-----|\n")
	for _, m := range r.byLanguage() {
		fmt.Fprintf(&b, "| %s | %d | %.3f | %.3f | %.3f |\n", m.label, m.n, m.ndcg, m.recall, m.mrr)
	}

	g := mean(r.Cells)
	b.WriteString("\n### Grand mean\n\n")
	b.WriteString("| cells | NDCG | Recall | MRR |\n")
	b.WriteString("|-------|------|--------|-----|\n")
	fmt.Fprintf(&b, "| %d | %.3f | %.3f | %.3f |\n", g.n, g.ndcg, g.recall, g.mrr)
	return b.String()
}

// JSON serializes the report (the flat cell list) as indented JSON — this is
// the committed baseline format.
func (r CorpusReport) JSON() ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}

// Regression is a single metric of a single (repo, set) cell that dropped
// beyond tolerance versus a reference report.
type Regression struct {
	Repo, Set, Metric string
	Was, Now          float64
}

func (r Regression) String() string {
	return fmt.Sprintf("%s/%s %s regressed: was %.3f, now %.3f (delta %.3f)",
		r.Repo, r.Set, r.Metric, r.Was, r.Now, r.Was-r.Now)
}

// Regressions returns the per-cell metrics that dropped by more than tol versus
// ref. Cells are matched by (repo, set); a cell present in ref but missing from
// the current report is reported as a regression on every metric (it vanished).
// A cell new in the current report is ignored (nothing to compare against).
func (r CorpusReport) Regressions(ref CorpusReport, tol float64) []Regression {
	cur := make(map[string]LabeledReport, len(r.Cells))
	for _, c := range r.Cells {
		cur[c.Repo+"\x00"+c.Set] = c
	}
	var out []Regression
	for _, rc := range ref.Cells {
		key := rc.Repo + "\x00" + rc.Set
		cc, ok := cur[key]
		if !ok {
			out = append(out, Regression{rc.Repo, rc.Set, "missing-cell", 1, 0})
			continue
		}
		for _, m := range []struct {
			name     string
			was, now float64
		}{
			{"NDCG", rc.Report.MeanNDCG, cc.Report.MeanNDCG},
			{"Recall", rc.Report.MeanRecall, cc.Report.MeanRecall},
			{"MRR", rc.Report.MRR, cc.Report.MRR},
		} {
			if m.was-m.now > tol {
				out = append(out, Regression{rc.Repo, rc.Set, m.name, m.was, m.now})
			}
		}
	}
	return out
}

// LoadReport reads a committed CorpusReport baseline from JSON.
func LoadReport(data []byte) (CorpusReport, error) {
	var r CorpusReport
	if err := json.Unmarshal(data, &r); err != nil {
		return CorpusReport{}, fmt.Errorf("parse corpus baseline: %w", err)
	}
	return r, nil
}
