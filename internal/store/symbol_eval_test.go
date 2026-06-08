package store

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// Symbol-query definition-boost eval (#146).
//
// The definition boost in ApplyLocalRerank multiplies declaration-kind chunks
// (function/method/class/struct/type) for queries classified as symbol lookups,
// lifting a symbol's declaration over window/doc fragments that merely mention
// it. lean-ctx uses 3.0×; dex shipped 1.5×. This harness picks the constant
// with data instead of copying it: it sweeps the multiplier over a fixed eval
// set and reports recall@1 / recall@3 / MRR of the target, split by query class.
//
// Two query classes exercise the trade-off:
//   - "def":     the target IS a declaration-kind chunk. A higher boost helps —
//                it pulls the declaration over the doc/window chunks.
//   - "constvar": the target is a top-level const/var, which dex chunks as an
//                "orphan" (NOT a declaration kind, so unboosted). A too-high
//                boost HURTS here: it lifts an unrelated declaration above the
//                const's real definition site.
//
// The optimum balances both, so the eval is not trivially "boost = infinity".

type symbolCorpusEntry struct {
	Path    string   `json:"path"`
	Kind    string   `json:"kind"`
	Name    string   `json:"name"`
	Content string   `json:"content"`
	Tags    []string `json:"tags"`
}

type symbolQueryEntry struct {
	Name     string   `json:"name"`
	Text     string   `json:"text"`
	Tags     []string `json:"tags"`
	K        int      `json:"k"`
	Class    string   `json:"class"`
	Relevant string   `json:"relevant"`
}

func loadSymbolCorpus(t *testing.T, path string) []symbolCorpusEntry {
	t.Helper()
	var out []symbolCorpusEntry
	loadJSONL(t, path, func(line string) {
		var e symbolCorpusEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("parse %s: %v: %s", path, err, line)
		}
		out = append(out, e)
	})
	return out
}

func loadSymbolQueries(t *testing.T, path string) []symbolQueryEntry {
	t.Helper()
	var out []symbolQueryEntry
	loadJSONL(t, path, func(line string) {
		var e symbolQueryEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("parse %s: %v: %s", path, err, line)
		}
		out = append(out, e)
	})
	return out
}

func indexSymbolCorpus(t *testing.T, st *Store, corpus []symbolCorpusEntry) {
	t.Helper()
	ctx := context.Background()
	rows := make([]PendingChunk, len(corpus))
	for i, c := range corpus {
		sum := sha1.Sum([]byte(c.Content))
		rows[i] = PendingChunk{
			Path:       c.Path,
			Kind:       c.Kind,
			Name:       c.Name,
			StartLine:  1,
			EndLine:    1,
			ContentSHA: hex.EncodeToString(sum[:]),
			Content:    c.Content,
			Vec:        embedRegressionTags(c.Tags),
		}
	}
	if err := st.UpsertMany(ctx, rows, time.Now()); err != nil {
		t.Fatalf("upsert symbol corpus: %v", err)
	}
}

// rankOfPath returns the 1-based rank of path in hits, or 0 if absent.
func rankOfPath(hits []Hit, path string) int {
	for i, h := range hits {
		if h.Path == path {
			return i + 1
		}
	}
	return 0
}

// evalAgg accumulates recall/MRR over a set of queries.
type evalAgg struct {
	n, hit1, hit3 int
	mrrSum        float64
}

func (a *evalAgg) add(rank int) {
	a.n++
	if rank == 1 {
		a.hit1++
	}
	if rank >= 1 && rank <= 3 {
		a.hit3++
	}
	if rank >= 1 {
		a.mrrSum += 1.0 / float64(rank)
	}
}

func (a evalAgg) recall1() float64 {
	if a.n == 0 {
		return 0
	}
	return float64(a.hit1) / float64(a.n)
}
func (a evalAgg) recall3() float64 {
	if a.n == 0 {
		return 0
	}
	return float64(a.hit3) / float64(a.n)
}
func (a evalAgg) mrr() float64 {
	if a.n == 0 {
		return 0
	}
	return a.mrrSum / float64(a.n)
}

func TestSymbolDefinitionBoostEval(t *testing.T) {
	corpus := loadSymbolCorpus(t, "testdata/symboleval/corpus.jsonl")
	queries := loadSymbolQueries(t, "testdata/symboleval/queries.jsonl")
	if len(corpus) == 0 || len(queries) == 0 {
		t.Fatalf("empty eval set: %d corpus, %d queries", len(corpus), len(queries))
	}

	ctx := context.Background()
	st, err := OpenWith(ctx, filepath.Join(t.TempDir(), "symboleval.db"), Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	indexSymbolCorpus(t, st, corpus)

	// Every query in the set must classify as a symbol lookup, else the
	// definition boost never fires and the eval measures nothing.
	for _, q := range queries {
		if classifyQueryType(q.Text) != querySymbol {
			t.Fatalf("query %q (%q) does not classify as querySymbol — eval invalid", q.Name, q.Text)
		}
	}

	// Fetch the fused (pre-rerank) pool once per query; the boost sweep then
	// reranks copies of that fixed pool, isolating the boost's effect from any
	// retrieval variance.
	fusedByQuery := make([][]Hit, len(queries))
	for i, q := range queries {
		hits, err := st.SearchFused(ctx, embedRegressionTags(q.Tags), q.Text, q.K)
		if err != nil {
			t.Fatalf("SearchFused %q: %v", q.Name, err)
		}
		fusedByQuery[i] = hits
	}

	boosts := []float64{1.0, 1.5, 2.0, 2.5, 3.0, 4.0, 6.0}
	classes := []string{"buried_def", "def", "constvar"}

	type row struct {
		boost   float64
		all     evalAgg
		byClass map[string]*evalAgg
	}
	results := make([]row, 0, len(boosts))

	// rankAt reruns the local rerank at a given boost over a copy of the fused
	// pool and returns the target's rank.
	rankAt := func(fused []Hit, q symbolQueryEntry, boost float64) int {
		cp := make([]Hit, len(fused))
		copy(cp, fused)
		ranked := ApplyLocalRerank(cp, true, boost)
		return rankOfPath(ranked, q.Relevant)
	}

	for _, boost := range boosts {
		r := row{boost: boost, byClass: map[string]*evalAgg{}}
		for _, c := range classes {
			r.byClass[c] = &evalAgg{}
		}
		for i, q := range queries {
			rank := rankAt(fusedByQuery[i], q, boost)
			r.all.add(rank)
			if agg, ok := r.byClass[q.Class]; ok {
				agg.add(rank)
			} else {
				t.Fatalf("query %q has unknown class %q", q.Name, q.Class)
			}
		}
		results = append(results, r)
	}

	// Report table.
	var b []byte
	b = fmt.Appendf(b, "\nSymbol definition-boost sweep (recall@1 / recall@3 / MRR), %d queries\n", len(queries))
	b = fmt.Appendf(b, "  default boost = %.1f×\n", float64(defaultDefinitionBoost))
	b = fmt.Appendf(b, "  %-6s | %-20s", "boost", "ALL")
	for _, c := range classes {
		b = fmt.Appendf(b, " | %-20s", c)
	}
	b = fmt.Appendf(b, "\n")
	for _, r := range results {
		b = fmt.Appendf(b, "  %-6.1f | %.2f / %.2f / %.3f", r.boost, r.all.recall1(), r.all.recall3(), r.all.mrr())
		for _, c := range classes {
			a := r.byClass[c]
			b = fmt.Appendf(b, " | %.2f / %.2f / %.3f", a.recall1(), a.recall3(), a.mrr())
		}
		b = fmt.Appendf(b, "\n")
	}
	t.Log(string(b))

	// Per-query rank at the shipped default, for transparency.
	var pq []byte
	pq = fmt.Appendf(pq, "\nPer-query target rank at default boost %.1f× (vs no-boost):\n", float64(defaultDefinitionBoost))
	for i, q := range queries {
		pq = fmt.Appendf(pq, "  %-16s [%-10s] rank=%d  (no-boost rank=%d)\n",
			q.Name, q.Class,
			rankAt(fusedByQuery[i], q, float64(defaultDefinitionBoost)),
			rankAt(fusedByQuery[i], q, 1.0))
	}
	t.Log(string(pq))

	// Locate baseline (no boost) and the shipped-default rows.
	var baseline, def row
	bestMRR := -1.0
	var bestBoosts []float64
	for _, r := range results {
		if r.boost == 1.0 {
			baseline = r
		}
		if r.boost == float64(defaultDefinitionBoost) {
			def = r
		}
		if r.all.mrr() > bestMRR+1e-9 {
			bestMRR = r.all.mrr()
			bestBoosts = []float64{r.boost}
		} else if r.all.mrr() > bestMRR-1e-9 {
			bestBoosts = append(bestBoosts, r.boost)
		}
	}

	// Guardrail 1: the boost must demonstrably HELP the buried-declaration
	// class — that is its entire purpose. If it doesn't, the eval set isn't
	// exercising the mechanism and any chosen value is unjustified.
	if def.byClass["buried_def"].mrr() <= baseline.byClass["buried_def"].mrr()+1e-9 {
		t.Errorf("default boost %.1f× does not improve buried-def MRR over no-boost: %.3f <= %.3f",
			float64(defaultDefinitionBoost),
			def.byClass["buried_def"].mrr(), baseline.byClass["buried_def"].mrr())
	}

	// Guardrail 2: the shipped default must be the data-optimal choice — its
	// overall MRR must equal the best MRR observed across the whole sweep.
	// This turns the eval into a regression test: changing defaultDefinitionBoost
	// to a worse value (e.g. blindly copying lean-ctx's 3.0 if it loses const/var
	// recall, or dropping back toward 1.0) fails the build.
	if def.all.mrr() < bestMRR-1e-9 {
		t.Errorf("default boost %.1f× is not data-optimal: overall MRR %.3f < best %.3f (best at boosts %v)",
			float64(defaultDefinitionBoost), def.all.mrr(), bestMRR, bestBoosts)
	}
}
