package retrieve

import (
	"reflect"
	"testing"
)

func TestSelectMaxCoverage_GreedyAndNonRedundant(t *testing.T) {
	// A covers {x,y} cost 2 (ratio 1.0); B covers {y,z} cost 1 (ratio 2.0);
	// C covers {z} cost 1 (fully redundant after B). Greedy: B first (highest
	// ratio), then A (adds x — the only uncovered key), then C adds nothing → skipped.
	items := []Coverable{
		{Keys: []string{"x", "y"}, Cost: 2}, // 0: A
		{Keys: []string{"y", "z"}, Cost: 1}, // 1: B
		{Keys: []string{"z"}, Cost: 1},      // 2: C — redundant once B is in
	}
	got := SelectMaxCoverage(items, 0)
	want := []int{1, 0} // B then A; C never adds new coverage
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestSelectMaxCoverage_BudgetCutoff(t *testing.T) {
	items := []Coverable{
		{Keys: []string{"a"}, Cost: 3}, // 0
		{Keys: []string{"b"}, Cost: 3}, // 1
		{Keys: []string{"c"}, Cost: 3}, // 2
	}
	// Budget 5 admits only one item (each costs 3; after one, 2 remain < 3).
	got := SelectMaxCoverage(items, 5)
	if len(got) != 1 {
		t.Fatalf("budget 5 should admit exactly 1 item, got %v", got)
	}
}

func TestSelectMaxCoverage_SkipsZeroCoverageAndZeroCost(t *testing.T) {
	items := []Coverable{
		{Keys: nil, Cost: 1},           // covers nothing → skipped
		{Keys: []string{"a"}, Cost: 0}, // zero cost → skipped
		{Keys: []string{"a"}, Cost: 1}, // the only valid pick
	}
	got := SelectMaxCoverage(items, 0)
	if !reflect.DeepEqual(got, []int{2}) {
		t.Fatalf("order = %v, want [2]", got)
	}
}

func TestCoverageOrder_RanksByKeywordCoverage(t *testing.T) {
	syms := []SymbolHit{
		{QualifiedName: "pkg.Unrelated", StartLine: 1, EndLine: 30},                                   // 0: covers none
		{QualifiedName: "pkg.ParseConfig", Signature: "func ParseConfig()", StartLine: 1, EndLine: 5}, // 1: covers parse,config cheaply
		{QualifiedName: "pkg.Config", StartLine: 1, EndLine: 50},                                      // 2: covers config, expensive
	}
	order := coverageOrder(syms, []string{"parse", "config"}, nil)
	if len(order) == 0 || order[0] != 1 {
		t.Fatalf("expected the cheap two-keyword cover (idx 1) first, got %v", order)
	}
	// idx 0 covers nothing, so it must not appear.
	for _, i := range order {
		if i == 0 {
			t.Fatalf("zero-coverage symbol 0 should be excluded, got %v", order)
		}
	}
}

func TestCoverageOrder_NoKeywordsIsNaturalOrder(t *testing.T) {
	syms := []SymbolHit{{QualifiedName: "A"}, {QualifiedName: "B"}, {QualifiedName: "C"}}
	got := coverageOrder(syms, nil, nil)
	if !reflect.DeepEqual(got, []int{0, 1, 2}) {
		t.Fatalf("no keywords should give natural order, got %v", got)
	}
}
