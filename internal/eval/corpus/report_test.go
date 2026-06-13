package corpus

import (
	"testing"

	"github.com/alehatsman/dex/internal/eval"
)

func cell(repo, lang, set string, ndcg, recall, mrr float64) LabeledReport {
	return LabeledReport{
		Repo: repo, Lang: lang, Set: set,
		Report: eval.Report{K: 10, N: 20, MeanNDCG: ndcg, MeanRecall: recall, MRR: mrr},
	}
}

func TestRollups(t *testing.T) {
	rep := Compute([]LabeledReport{
		cell("flask", "python", "curated:flask.json", 0.50, 0.60, 0.70),
		cell("flask", "python", "git-history", 0.30, 0.40, 0.50),
		cell("gin", "go", "git-history", 0.10, 0.20, 0.30),
	}, 10)

	langs := rep.byLanguage()
	if len(langs) != 2 {
		t.Fatalf("languages = %d, want 2", len(langs))
	}
	// python row is the mean of its two cells: ndcg (0.5+0.3)/2 = 0.4
	var py meanRow
	for _, m := range langs {
		if m.label == "python" {
			py = m
		}
	}
	if py.n != 2 || !approx(py.ndcg, 0.40) {
		t.Errorf("python rollup = %+v, want n=2 ndcg=0.40", py)
	}
	g := mean(rep.Cells)
	if !approx(g.ndcg, (0.50+0.30+0.10)/3) {
		t.Errorf("grand mean ndcg = %.4f, want 0.30", g.ndcg)
	}
}

func TestRegressionsPerCell(t *testing.T) {
	ref := Compute([]LabeledReport{
		cell("flask", "python", "git-history", 0.50, 0.60, 0.70),
		cell("gin", "go", "git-history", 0.40, 0.40, 0.40),
	}, 10)

	// gin NDCG drops 0.10 (> tol); flask unchanged; gin Recall/MRR steady.
	cur := Compute([]LabeledReport{
		cell("flask", "python", "git-history", 0.50, 0.60, 0.70),
		cell("gin", "go", "git-history", 0.30, 0.40, 0.40),
	}, 10)

	regs := cur.Regressions(ref, 0.02)
	if len(regs) != 1 {
		t.Fatalf("regressions = %d (%v), want 1", len(regs), regs)
	}
	if regs[0].Repo != "gin" || regs[0].Metric != "NDCG" {
		t.Errorf("regression = %+v, want gin/NDCG", regs[0])
	}
}

func TestRegressionsNoneWithinTol(t *testing.T) {
	ref := Compute([]LabeledReport{cell("flask", "python", "git-history", 0.50, 0.60, 0.70)}, 10)
	cur := Compute([]LabeledReport{cell("flask", "python", "git-history", 0.49, 0.59, 0.69)}, 10)
	if regs := cur.Regressions(ref, 0.02); len(regs) != 0 {
		t.Errorf("regressions = %v, want none (within tol)", regs)
	}
}

func TestRegressionsMissingCell(t *testing.T) {
	ref := Compute([]LabeledReport{
		cell("flask", "python", "git-history", 0.50, 0.60, 0.70),
		cell("gin", "go", "git-history", 0.40, 0.40, 0.40),
	}, 10)
	cur := Compute([]LabeledReport{cell("flask", "python", "git-history", 0.50, 0.60, 0.70)}, 10)
	regs := cur.Regressions(ref, 0.02)
	if len(regs) != 1 || regs[0].Repo != "gin" || regs[0].Metric != "missing-cell" {
		t.Fatalf("regressions = %v, want one gin/missing-cell", regs)
	}
}

func TestReportJSONRoundTrip(t *testing.T) {
	rep := Compute([]LabeledReport{cell("flask", "python", "git-history", 0.5, 0.6, 0.7)}, 10)
	data, err := rep.JSON()
	if err != nil {
		t.Fatal(err)
	}
	got, err := LoadReport(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Cells) != 1 || got.Cells[0].Report.MeanNDCG != 0.5 {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}

func approx(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}
