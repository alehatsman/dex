package trace

import (
	"strings"
	"testing"
)

func sampleSuite() Suite {
	return Compute([]Report{
		{Repo: "gin", Lang: "go", Set: "core", Probes: 10, Unresolved: 0, MacroPrecision: 0.90, MacroRecall: 0.95, MacroF1: 0.92},
		{Repo: "flask", Lang: "python", Set: "core", Probes: 8, Unresolved: 2, MacroPrecision: 0.60, MacroRecall: 0.50, MacroF1: 0.54},
		{Repo: "ripgrep", Lang: "rust", Set: "core", Probes: 6, Unresolved: 1, MacroPrecision: 0.70, MacroRecall: 0.40, MacroF1: 0.51},
	})
}

func TestSuiteMarkdownRollups(t *testing.T) {
	md := sampleSuite().Markdown()
	for _, want := range []string{
		"3 cells",
		"| gin | go | core | 10 | 0 | 0.900 | 0.950 | 0.920 |",
		"Per-language",
		"Grand mean",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q\n---\n%s", want, md)
		}
	}
}

func TestSuiteJSONRoundTrip(t *testing.T) {
	s := sampleSuite()
	data, err := s.JSON()
	if err != nil {
		t.Fatal(err)
	}
	got, err := LoadSuite(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Cells) != 3 {
		t.Fatalf("round-trip cells = %d, want 3", len(got.Cells))
	}
	if got.Cells[0].Repo != "gin" || got.Cells[0].MacroF1 != 0.92 {
		t.Errorf("round-trip cell[0] = %+v", got.Cells[0])
	}
}

func TestRegressionsDetectDropAndMissing(t *testing.T) {
	ref := sampleSuite()
	// degrade flask recall past tol; drop ripgrep entirely; gin unchanged.
	cur := Compute([]Report{
		{Repo: "gin", Lang: "go", Set: "core", MacroPrecision: 0.90, MacroRecall: 0.95, MacroF1: 0.92},
		{Repo: "flask", Lang: "python", Set: "core", MacroPrecision: 0.60, MacroRecall: 0.30, MacroF1: 0.40},
	})
	regs := cur.Regressions(ref, 0.05)

	var sawFlaskRecall, sawRipgrepMissing, sawGin bool
	for _, r := range regs {
		switch {
		case r.Repo == "flask" && r.Metric == "Recall":
			sawFlaskRecall = true
		case r.Repo == "ripgrep" && r.Metric == "missing-cell":
			sawRipgrepMissing = true
		case r.Repo == "gin":
			sawGin = true
		}
	}
	if !sawFlaskRecall {
		t.Error("expected flask Recall regression")
	}
	if !sawRipgrepMissing {
		t.Error("expected ripgrep missing-cell regression")
	}
	if sawGin {
		t.Error("gin unchanged but flagged as regression")
	}
}

func TestRegressionsWithinTolerance(t *testing.T) {
	ref := sampleSuite()
	// drop everything by less than tol — must NOT trip.
	var cells []Report
	for _, c := range ref.Cells {
		c.MacroPrecision -= 0.04
		c.MacroRecall -= 0.04
		c.MacroF1 -= 0.04
		cells = append(cells, c)
	}
	if regs := Compute(cells).Regressions(ref, 0.05); len(regs) != 0 {
		t.Errorf("drop < tol should not trip, got %v", regs)
	}
}
