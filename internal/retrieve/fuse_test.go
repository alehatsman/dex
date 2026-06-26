package retrieve

import (
	"testing"

	"github.com/alehatsman/dex/internal/store"
)

func TestFuseWithSymbols_SemanticOnlyWhenNoSymbols(t *testing.T) {
	sem := []store.Hit{
		{Path: "a.go", StartLine: 1, Score: 0.9},
		{Path: "b.go", StartLine: 1, Score: 0.7},
	}
	out := FuseWithSymbols(sem, nil, 5)
	if len(out) != 2 {
		t.Fatalf("want 2 hits, got %d", len(out))
	}
	// Semantic-only: score(d) = 1/(60+rank). Rank-1 beats rank-2.
	if out[0].Path != "a.go" {
		t.Errorf("first hit should be a.go, got %s", out[0].Path)
	}
}

func TestFuseWithSymbols_SymbolOnlyHitsGetScore1(t *testing.T) {
	sem := []store.Hit{{Path: "a.go", StartLine: 1, Score: 0.9}}
	sym := []store.Hit{{Path: "b.go", StartLine: 10, Score: 0}} // no cosine score
	out := FuseWithSymbols(sem, sym, 5)
	for _, h := range out {
		if h.Path == "b.go" && h.Score != 1.0 {
			t.Errorf("symbol-only hit should have Score=1.0, got %f", h.Score)
		}
	}
}

func TestFuseWithSymbols_OverlapBoostRank(t *testing.T) {
	// c.go appears in both lanes at rank 1 — should beat a.go (semantic-only rank 1).
	sem := []store.Hit{
		{Path: "a.go", StartLine: 1, Score: 0.95},
		{Path: "c.go", StartLine: 5, Score: 0.5},
	}
	sym := []store.Hit{
		{Path: "c.go", StartLine: 5},
		{Path: "b.go", StartLine: 1},
	}
	out := FuseWithSymbols(sem, sym, 5)
	if len(out) == 0 {
		t.Fatal("expected hits")
	}
	// c.go: 1/(60+2) + 1/(60+1) = 0.01613 + 0.01639 = 0.03252
	// a.go: 1/(60+1)             = 0.01639
	// c.go must rank first.
	if out[0].Path != "c.go" {
		t.Errorf("overlapping hit should rank first, got %s", out[0].Path)
	}
}

func TestFuseWithSymbols_RRFScoreSet(t *testing.T) {
	sem := []store.Hit{{Path: "a.go", StartLine: 1, Score: 0.9}}
	sym := []store.Hit{{Path: "b.go", StartLine: 1}}
	out := FuseWithSymbols(sem, sym, 5)
	for _, h := range out {
		if h.RRFScore == 0 {
			t.Errorf("hit %s:%d should have non-zero RRFScore", h.Path, h.StartLine)
		}
	}
}

func TestFuseWithSymbols_TopN(t *testing.T) {
	sem := make([]store.Hit, 10)
	for i := range sem {
		sem[i] = store.Hit{Path: "sem.go", StartLine: i + 1, Score: float32(10-i) / 10}
	}
	out := FuseWithSymbols(sem, nil, 3)
	if len(out) != 3 {
		t.Errorf("want 3 hits, got %d", len(out))
	}
}

func TestFuseWithSymbols_LaneProvenance(t *testing.T) {
	// a.go: semantic-only (carries vector+bm25 from the store).
	// c.go: in both lanes — must gain the symbol lane on top of vector+bm25.
	// b.go: symbol-only — must carry just the symbol lane.
	sem := []store.Hit{
		{Path: "a.go", StartLine: 1, Score: 0.9, Lanes: store.LaneSet(0).With(store.LaneVector).With(store.LaneBM25)},
		{Path: "c.go", StartLine: 5, Score: 0.5, Lanes: store.LaneSet(0).With(store.LaneVector).With(store.LaneBM25)},
	}
	sym := []store.Hit{
		{Path: "c.go", StartLine: 5},
		{Path: "b.go", StartLine: 1},
	}
	out := FuseWithSymbols(sem, sym, 5)
	byPath := map[string]store.Hit{}
	for _, h := range out {
		byPath[h.Path] = h
	}
	if got := byPath["a.go"].Lanes; got.Has(store.LaneSymbol) {
		t.Errorf("a.go should not carry the symbol lane, got %v", got.Names())
	}
	if got := byPath["c.go"].Lanes; !(got.Has(store.LaneVector) && got.Has(store.LaneBM25) && got.Has(store.LaneSymbol)) {
		t.Errorf("c.go should carry vector+bm25+symbol, got %v", got.Names())
	}
	if got := byPath["b.go"].Lanes; !got.Has(store.LaneSymbol) || got.Has(store.LaneVector) {
		t.Errorf("b.go should carry symbol only, got %v", got.Names())
	}
}

func TestFuseWithSymbols_DeduplicatesByPathLine(t *testing.T) {
	h := store.Hit{Path: "x.go", StartLine: 42, Score: 0.8}
	out := FuseWithSymbols([]store.Hit{h}, []store.Hit{h}, 5)
	count := 0
	for _, r := range out {
		if r.Path == "x.go" && r.StartLine == 42 {
			count++
		}
	}
	if count != 1 {
		t.Errorf("duplicate (path,line) should appear once, got %d", count)
	}
}
