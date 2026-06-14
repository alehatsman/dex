package trace

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Suite aggregates the per-(repo, set) cells of a trace run. It carries the raw
// cells; per-language and grand-mean rollups are derived on render so the JSON
// baseline stays a flat, diffable list of cells — same shape choice as the
// corpus retrieval baseline.
type Suite struct {
	Cells []Report `json:"cells"`
}

// Compute wraps scored cells into a Suite.
func Compute(cells []Report) Suite { return Suite{Cells: cells} }

type meanRow struct {
	label                 string
	n                     int // number of cells averaged
	probes                int // total probes across the cells
	unresolved            int // total unresolved probes across the cells
	precision, recall, f1 float64
}

func mean(cells []Report) meanRow {
	var r meanRow
	if len(cells) == 0 {
		return r
	}
	for _, c := range cells {
		r.precision += c.MacroPrecision
		r.recall += c.MacroRecall
		r.f1 += c.MacroF1
		r.probes += c.Probes
		r.unresolved += c.Unresolved
	}
	n := float64(len(cells))
	r.n = len(cells)
	r.precision /= n
	r.recall /= n
	r.f1 /= n
	return r
}

// byLanguage groups cells by primary language, returning rows sorted by label.
// Each language's metrics are the unweighted mean of its cells (one vote per
// cell) — this is the headline #468 number: does the uniform substrate hold
// per-language precision/recall against the hand-verified gold.
func (s Suite) byLanguage() []meanRow {
	groups := map[string][]Report{}
	for _, c := range s.Cells {
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
func (s Suite) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Cross-language Trace Eval — %d cells\n\n", len(s.Cells))

	b.WriteString("| repo | lang | set | probes | unres | Precision | Recall | F1 |\n")
	b.WriteString("|------|------|-----|--------|-------|-----------|--------|----|\n")
	cells := append([]Report(nil), s.Cells...)
	sort.Slice(cells, func(i, j int) bool {
		if cells[i].Repo != cells[j].Repo {
			return cells[i].Repo < cells[j].Repo
		}
		return cells[i].Set < cells[j].Set
	})
	for _, c := range cells {
		fmt.Fprintf(&b, "| %s | %s | %s | %d | %d | %.3f | %.3f | %.3f |\n",
			c.Repo, c.Lang, c.Set, c.Probes, c.Unresolved, c.MacroPrecision, c.MacroRecall, c.MacroF1)
	}

	b.WriteString("\n### Per-language (unweighted mean of cells)\n\n")
	b.WriteString("| lang | cells | probes | unres | Precision | Recall | F1 |\n")
	b.WriteString("|------|-------|--------|-------|-----------|--------|----|\n")
	for _, m := range s.byLanguage() {
		fmt.Fprintf(&b, "| %s | %d | %d | %d | %.3f | %.3f | %.3f |\n",
			m.label, m.n, m.probes, m.unresolved, m.precision, m.recall, m.f1)
	}

	g := mean(s.Cells)
	b.WriteString("\n### Grand mean\n\n")
	b.WriteString("| cells | probes | unres | Precision | Recall | F1 |\n")
	b.WriteString("|-------|--------|-------|-----------|--------|----|\n")
	fmt.Fprintf(&b, "| %d | %d | %d | %.3f | %.3f | %.3f |\n", g.n, g.probes, g.unresolved, g.precision, g.recall, g.f1)
	return b.String()
}

// JSON serializes the suite (the flat cell list) as indented JSON — the
// committed baseline format.
func (s Suite) JSON() ([]byte, error) { return json.MarshalIndent(s, "", "  ") }

// Regression is a single metric of a single (repo, set) cell that dropped
// beyond tolerance versus a reference suite.
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
// the current suite is reported as a vanished cell. A cell new in the current
// suite is ignored (nothing to compare against). This is the #468 gate: run it
// before the extractor swap to capture ref, after to assert no per-cell drop.
func (s Suite) Regressions(ref Suite, tol float64) []Regression {
	cur := make(map[string]Report, len(s.Cells))
	for _, c := range s.Cells {
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
			{"Precision", rc.MacroPrecision, cc.MacroPrecision},
			{"Recall", rc.MacroRecall, cc.MacroRecall},
			{"F1", rc.MacroF1, cc.MacroF1},
		} {
			if m.was-m.now > tol {
				out = append(out, Regression{rc.Repo, rc.Set, m.name, m.was, m.now})
			}
		}
	}
	return out
}

// LoadSuite reads a committed Suite baseline from JSON.
func LoadSuite(data []byte) (Suite, error) {
	var s Suite
	if err := json.Unmarshal(data, &s); err != nil {
		return Suite{}, fmt.Errorf("parse trace baseline: %w", err)
	}
	return s, nil
}
