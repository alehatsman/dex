package skew

import (
	"strings"
	"testing"
)

func suite2() Suite {
	return NewSuite([]Cell{
		{Repo: "gotify", Report: Report{
			TotalNodes: 100,
			Languages: []LangStat{
				{Language: "go", SkewRatio: 1.08},
				{Language: "typescript", SkewRatio: 0.69},
			},
		}},
	})
}

func TestSuite_JSONRoundTrip(t *testing.T) {
	s := suite2()
	data, err := s.JSON()
	if err != nil {
		t.Fatal(err)
	}
	back, err := LoadSuite(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Cells) != 1 || back.Cells[0].Repo != "gotify" {
		t.Fatalf("roundtrip lost cell: %+v", back)
	}
	if got := back.Cells[0].Report.Languages[1].SkewRatio; got != 0.69 {
		t.Errorf("ts skew = %v, want 0.69", got)
	}
}

func TestSuite_DriftWithinTolPasses(t *testing.T) {
	base := suite2()
	cur := NewSuite([]Cell{
		{Repo: "gotify", Report: Report{Languages: []LangStat{
			{Language: "go", SkewRatio: 1.10},         // +0.02, within tol
			{Language: "typescript", SkewRatio: 0.66}, // -0.03, within tol
		}}},
	})
	if d := cur.Drift(base, 0.05); len(d) != 0 {
		t.Errorf("expected no drift within tol, got %v", d)
	}
}

func TestSuite_DriftBeyondTolReported(t *testing.T) {
	base := suite2()
	cur := NewSuite([]Cell{
		{Repo: "gotify", Report: Report{Languages: []LangStat{
			{Language: "go", SkewRatio: 1.08},         // unchanged
			{Language: "typescript", SkewRatio: 0.90}, // +0.21 — a resolver fix would move this
		}}},
	})
	d := cur.Drift(base, 0.05)
	if len(d) != 1 || d[0].Language != "typescript" {
		t.Fatalf("expected typescript drift, got %v", d)
	}
	if d[0].Old != 0.69 || d[0].New != 0.90 {
		t.Errorf("drift values = %v→%v, want 0.69→0.90", d[0].Old, d[0].New)
	}
}

// A language dropping out of a baseline-known repo is a regression (Old→0).
func TestSuite_DriftDroppedLanguage(t *testing.T) {
	base := suite2()
	cur := NewSuite([]Cell{
		{Repo: "gotify", Report: Report{Languages: []LangStat{
			{Language: "go", SkewRatio: 1.08},
		}}},
	})
	d := cur.Drift(base, 0.05)
	if len(d) != 1 || d[0].Language != "typescript" || d[0].New != 0 {
		t.Fatalf("expected dropped typescript (New=0), got %v", d)
	}
}

func TestSuite_Markdown(t *testing.T) {
	md := suite2().Markdown()
	for _, want := range []string{"gotify", "| go |", "| typescript |", "skew"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q:\n%s", want, md)
		}
	}
}
