package store

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"testing"
	"time"
)

// openQuant opens a store at dbPath under the given vector-quant mode
// ("int8" or "" for float32). The caller controls the path so a store can
// be closed and reopened under a different mode to exercise the rebuild.
func openQuant(t *testing.T, dbPath, mode string) (*Store, context.Context) {
	t.Helper()
	ctx := context.Background()
	st, err := OpenWith(ctx, dbPath, Options{InfraOptions: InfraOptions{VectorQuant: mode}})
	if err != nil {
		t.Fatalf("OpenWith(%q): %v", mode, err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, ctx
}

// unitVec L2-normalizes v in place and returns it — embeddings are
// normalized upstream, and the int8 'unit' range assumes components ∈ [-1,1].
func unitVec(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	n := float32(math.Sqrt(sum))
	if n == 0 {
		return v
	}
	for i := range v {
		v[i] /= n
	}
	return v
}

// synthCorpus builds n deterministic unit vectors of the given dim via a
// tiny LCG — reproducible, no math/rand dependency, no test flakiness.
func synthCorpus(n, dim int, seed uint64) [][]float32 {
	out := make([][]float32, n)
	x := seed | 1
	next := func() float32 {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		// map to [-1,1)
		return float32(int64(x%2000))/1000.0 - 1.0
	}
	for i := range out {
		v := make([]float32, dim)
		for d := range v {
			v[d] = next()
		}
		out[i] = unitVec(v)
	}
	return out
}

func topIDs(scored []scored, k int) []int64 {
	if k > len(scored) {
		k = len(scored)
	}
	ids := make([]int64, k)
	for i := 0; i < k; i++ {
		ids[i] = scored[i].id
	}
	return ids
}

func overlap(a, b []int64) int {
	set := make(map[int64]struct{}, len(a))
	for _, id := range a {
		set[id] = struct{}{}
	}
	n := 0
	for _, id := range b {
		if _, ok := set[id]; ok {
			n++
		}
	}
	return n
}

// TestVectorQuantInt8RecallParity is the core quality gate: int8-quantized
// KNN must recover almost all of the float32 top-k. We build the same
// corpus into a float32 store (ground truth) and an int8 store, then
// compare scoreSemantic top-k over a batch of held-out queries. int8 scalar
// quantization is near-lossless at top-k, so we require high mean recall.
func TestVectorQuantInt8RecallParity(t *testing.T) {
	const (
		nDocs     = 120
		dim       = 32
		nQuery    = 40
		k         = 10
		minRecall = 0.90 // mean recall@k of int8 vs float32 ground truth
	)
	docs := synthCorpus(nDocs, dim, 0x9e3779b97f4a7c15)
	queries := synthCorpus(nQuery, dim, 0xdeadbeefcafef00d)
	now := time.Now()

	load := func(st *Store, ctx context.Context) {
		rows := make([]PendingChunk, nDocs)
		for i, v := range docs {
			rows[i] = PendingChunk{
				Path:       fmt.Sprintf("f%03d.go", i),
				Kind:       "fn",
				StartLine:  1,
				EndLine:    2,
				ContentSHA: fmt.Sprintf("h%03d", i),
				Content:    fmt.Sprintf("func F%03d() {}", i),
				Vec:        v,
			}
		}
		if err := st.UpsertMany(ctx, rows, now); err != nil {
			t.Fatal(err)
		}
	}

	dir := t.TempDir()
	f32, ctx := openQuant(t, filepath.Join(dir, "f32.db"), "")
	i8, _ := openQuant(t, filepath.Join(dir, "i8.db"), "int8")
	load(f32, ctx)
	load(i8, ctx)

	var sum float64
	for _, q := range queries {
		want, err := f32.scoreSemantic(ctx, q, k)
		if err != nil {
			t.Fatal(err)
		}
		got, err := i8.scoreSemantic(ctx, q, k)
		if err != nil {
			t.Fatal(err)
		}
		if len(want) == 0 {
			t.Fatal("float32 ground truth returned no hits")
		}
		r := float64(overlap(topIDs(want, k), topIDs(got, k))) / float64(len(topIDs(want, k)))
		sum += r
	}
	mean := sum / float64(len(queries))
	t.Logf("int8 mean recall@%d vs float32 ground truth = %.4f (dim=%d, docs=%d, queries=%d)", k, mean, dim, nDocs, nQuery)
	if mean < minRecall {
		t.Errorf("mean recall@%d = %.3f, want >= %.2f", k, mean, minRecall)
	}
}

// TestVectorQuantMetaRoundTrip verifies the encoding is recorded in meta and
// that flipping the mode on an existing index rebuilds chunk_vecs from the
// full-precision chunks.vec — search keeps working across the flip.
func TestVectorQuantMetaRoundTrip(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "rt.db")
	ctx := context.Background()
	now := time.Now()
	rows := []PendingChunk{
		{Path: "a.go", Kind: "fn", StartLine: 1, EndLine: 2, ContentSHA: "h1", Content: "func A(){}", Vec: unitVec([]float32{1, 0, 0, 0})},
		{Path: "b.go", Kind: "fn", StartLine: 1, EndLine: 2, ContentSHA: "h2", Content: "func B(){}", Vec: unitVec([]float32{0, 1, 0, 0})},
		{Path: "c.go", Kind: "fn", StartLine: 1, EndLine: 2, ContentSHA: "h3", Content: "func C(){}", Vec: unitVec([]float32{1, 1, 0, 0})},
	}

	// 1. Build as int8.
	st, err := OpenWith(ctx, dbPath, Options{InfraOptions: InfraOptions{VectorQuant: "int8"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertMany(ctx, rows, now); err != nil {
		t.Fatal(err)
	}
	assertMeta(t, st, ctx, metaVecQuant, "int8")
	assertTopHit(t, st, ctx, []float32{1, 0, 0, 0}, "a.go")
	_ = st.Close()

	// 2. Reopen as float32 — a mode flip must rebuild chunk_vecs (no reindex).
	st, err = OpenWith(ctx, dbPath, Options{InfraOptions: InfraOptions{VectorQuant: ""}})
	if err != nil {
		t.Fatal(err)
	}
	assertMeta(t, st, ctx, metaVecQuant, "float32")
	assertTopHit(t, st, ctx, []float32{1, 0, 0, 0}, "a.go")
	_ = st.Close()

	// 3. Reopen as int8 again — flips back and still searches.
	st, err = OpenWith(ctx, dbPath, Options{InfraOptions: InfraOptions{VectorQuant: "int8"}})
	if err != nil {
		t.Fatal(err)
	}
	assertMeta(t, st, ctx, metaVecQuant, "int8")
	assertTopHit(t, st, ctx, []float32{1, 0, 0, 0}, "a.go")
}

func assertMeta(t *testing.T, st *Store, ctx context.Context, key, want string) {
	t.Helper()
	var got string
	if err := st.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, key).Scan(&got); err != nil {
		t.Fatalf("read meta[%s]: %v", key, err)
	}
	if got != want {
		t.Errorf("meta[%s] = %q, want %q", key, got, want)
	}
}

func assertTopHit(t *testing.T, st *Store, ctx context.Context, q []float32, wantPath string) {
	t.Helper()
	hits, err := st.scoreSemantic(ctx, q, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatalf("no semantic hits for query")
	}
	paths, err := st.fetchPathsForIDs(ctx, []int64{hits[0].id})
	if err != nil {
		t.Fatal(err)
	}
	if got := paths[hits[0].id]; got != wantPath {
		t.Errorf("top hit = %q, want %q", got, wantPath)
	}
}
