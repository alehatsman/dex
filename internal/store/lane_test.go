package store

import (
	"reflect"
	"testing"
)

func TestLaneSet_NamesStableOrder(t *testing.T) {
	// Built out of declaration order; Names must still emit the canonical
	// vector→bm25→symbol→graph order.
	s := LaneSet(0).With(LaneGraph).With(LaneVector).With(LaneSymbol).With(LaneBM25)
	want := []string{"vector", "bm25", "symbol", "graph"}
	if got := s.Names(); !reflect.DeepEqual(got, want) {
		t.Errorf("Names() = %v, want %v", got, want)
	}
}

func TestLaneSet_EmptyIsNil(t *testing.T) {
	if got := LaneSet(0).Names(); got != nil {
		t.Errorf("empty LaneSet should render nil, got %v", got)
	}
}

func TestLaneSet_HasAndWith(t *testing.T) {
	s := LaneSet(0).With(LaneVector)
	if !s.Has(LaneVector) {
		t.Error("expected vector lane present")
	}
	if s.Has(LaneBM25) {
		t.Error("bm25 lane should be absent")
	}
	// With is idempotent — adding the same lane twice is a no-op union.
	if s.With(LaneVector) != s {
		t.Error("With(sameLane) should be idempotent")
	}
}

func TestFuseWithGraphNeighbors_LaneProvenance(t *testing.T) {
	// p.go is a primary hit (vector lane) that is ALSO a graph neighbor —
	// it must gain the graph lane. g.go is graph-only — graph lane only.
	primary := []Hit{{Path: "p.go", StartLine: 1, Lanes: LaneSet(0).With(LaneVector)}}
	graphHits := []Hit{
		{Path: "p.go", StartLine: 1},
		{Path: "g.go", StartLine: 1},
	}
	weights := map[string]float32{"p.go": 0.6, "g.go": 0.6}
	out := fuseWithGraphNeighbors(primary, graphHits, weights, 1.0, 10)
	byPath := map[string]Hit{}
	for _, h := range out {
		byPath[h.Path] = h
	}
	if got := byPath["p.go"].Lanes; !(got.Has(LaneVector) && got.Has(LaneGraph)) {
		t.Errorf("p.go should carry vector+graph, got %v", got.Names())
	}
	if got := byPath["g.go"].Lanes; !got.Has(LaneGraph) || got.Has(LaneVector) {
		t.Errorf("g.go should carry graph only, got %v", got.Names())
	}
}
