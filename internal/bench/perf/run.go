// Package perf provides an offline deterministic pipeline performance
// benchmark. All timed paths use synthetic data (no live embed/chat/rerank
// calls) so the bench runs with zero GPU and zero network — gateable on any
// box. GPU/network paths are report-only and stubbed with a TODO marker.
package perf

import (
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/alehatsman/dex/internal/compress"
	"github.com/alehatsman/dex/internal/store"
)

// RunResult holds timing statistics and metadata for one benchmark target.
type RunResult struct {
	Name         string        `json:"name"`
	CorpusSize   int           `json:"corpus_size,omitempty"` // chunk count (for scaling curves)
	Dim          int           `json:"dim,omitempty"`         // vector dim
	P50          time.Duration `json:"p50_ns"`
	P95          time.Duration `json:"p95_ns"`
	P99          time.Duration `json:"p99_ns"`
	Iterations   int           `json:"iterations"`
	StorageBytes int64         `json:"storage_bytes,omitempty"` // non-zero for storage probes
	ReportOnly   bool          `json:"report_only,omitempty"`   // true = not gated (GPU/network)
}

// Opts controls the benchmark run.
type Opts struct {
	// Iterations is the number of timed repetitions per benchmark target.
	// Default: 100.
	Iterations int
	// ScaleSizes is the set of corpus sizes used for KNN scaling curves.
	// Default: {1000, 5000, 20000, 50000, 100000}.
	ScaleSizes []int
	// Dim is the vector dimensionality to use for synthetic data.
	// Default: 1024 (matches nomic-embed-text / nomic-embed-code).
	Dim int
}

func (o *Opts) withDefaults() Opts {
	out := *o
	if out.Iterations <= 0 {
		out.Iterations = 100
	}
	if len(out.ScaleSizes) == 0 {
		out.ScaleSizes = []int{1000, 5000, 20000, 50000, 100000}
	}
	if out.Dim <= 0 {
		out.Dim = 1024
	}
	return out
}

// Run executes the full local-compute perf suite and returns the results.
// All operations use synthetic data; no live services are required.
func Run(opts Opts) ([]RunResult, error) {
	o := opts.withDefaults()
	ctx := context.Background()
	var results []RunResult

	// ── Compress-pass latency ────────────────────────────────────────────────
	results = append(results, benchCompressPasses(o)...)

	// ── KNN vector search scaling curves ────────────────────────────────────
	knnResults, err := benchKNNScaling(ctx, o)
	if err != nil {
		return nil, err
	}
	results = append(results, knnResults...)

	// ── BM25/FTS5 search ────────────────────────────────────────────────────
	bm25Results, err := benchBM25(ctx, o)
	if err != nil {
		return nil, err
	}
	results = append(results, bm25Results...)

	// ── Storage footprint ────────────────────────────────────────────────────
	storageResults, err := benchStorage(ctx, o)
	if err != nil {
		return nil, err
	}
	results = append(results, storageResults...)

	return results, nil
}

// ── compress pass latency ────────────────────────────────────────────────────

var compressSample = strings.Repeat(`func (s *Store) Search(ctx context.Context, queryVec []float32, queryText string, k int) ([]Hit, error) {
	if len(queryVec) == 0 {
		return s.scoreBM25(ctx, queryText, k)
	}
	hits, err := s.searchHybrid(ctx, queryVec, queryText, k)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	return hits, nil
}
`, 8)

func benchCompressPasses(o Opts) []RunResult {
	type passCase struct {
		name string
		fn   func() string
	}
	content := compressSample
	cases := []passCase{
		{"compress/aggressive", func() string { return compress.AggressiveCompress(content, ".go") }},
		{"compress/entropy", func() string {
			lines := strings.Split(content, "\n")
			return strings.Join(compress.EntropyFilter(lines, compress.EntropyThresholdStandard), "\n")
		}},
		{"compress/terse", func() string { r := compress.TerseCompress(content, compress.Level2); return r.Output }},
		{"compress/ib", func() string { return compress.CompressIB(content, 0.7) }},
		{"compress/codebook", func() string {
			cb := compress.BuildCodebook([]string{content})
			if cb.Empty() {
				return content
			}
			return cb.Legend() + "\n" + cb.Apply(content)
		}},
		{"compress/ngram_codebook", func() string { return compress.BuildNgramCodebook(content).ApplyWithLegend(content) }},
		{"compress/symmap", func() string { return compress.BuildSymbolMap(content).ApplyWithLegend(content) }},
	}

	var results []RunResult
	for _, c := range cases {
		rr := timeN(c.name, o.Iterations, c.fn)
		results = append(results, rr)
	}
	return results
}

// ── KNN scaling curves ───────────────────────────────────────────────────────

func benchKNNScaling(ctx context.Context, o Opts) ([]RunResult, error) {
	var results []RunResult
	for _, n := range o.ScaleSizes {
		st, _, cleanup, err := syntheticStore(ctx, n, o.Dim)
		if err != nil {
			return nil, err
		}
		q := syntheticVec(o.Dim, 0)
		rr := timeNErr(
			nameKNN(n, o.Dim),
			o.Iterations,
			func() error {
				_, searchErr := st.Search(ctx, q, "", 8)
				return searchErr
			},
		)
		rr.CorpusSize = n
		rr.Dim = o.Dim
		results = append(results, rr)
		cleanup()
	}
	return results, nil
}

// ── BM25 search ──────────────────────────────────────────────────────────────

func benchBM25(ctx context.Context, o Opts) ([]RunResult, error) {
	n := 5000
	st, _, cleanup, err := syntheticStore(ctx, n, o.Dim)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	rr := timeNErr("bm25/5k", o.Iterations, func() error {
		_, err := st.Search(ctx, nil, "queryText store search hybrid BM25", 8)
		return err
	})
	rr.CorpusSize = n
	return []RunResult{rr}, nil
}

// ── storage footprint ────────────────────────────────────────────────────────

func benchStorage(ctx context.Context, o Opts) ([]RunResult, error) {
	var results []RunResult
	for _, n := range []int{1000, 5000, 20000} {
		_, dbPath, cleanup, err := syntheticStore(ctx, n, o.Dim)
		if err != nil {
			return nil, err
		}
		fi, statErr := os.Stat(dbPath)
		cleanup()
		if statErr != nil {
			continue
		}
		results = append(results, RunResult{
			Name:         nameStorage(n, o.Dim),
			CorpusSize:   n,
			Dim:          o.Dim,
			StorageBytes: fi.Size(),
		})
	}
	return results, nil
}

// ── helpers ──────────────────────────────────────────────────────────────────

// syntheticStore creates a temporary store populated with n synthetic chunks of
// the given vector dimensionality. cleanup removes the temp directory.
// The returned dbPath is the on-disk path of the index file (for size probes).
func syntheticStore(ctx context.Context, n, dim int) (st *store.Store, dbPath string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "dex-bench-perf-*")
	if err != nil {
		return nil, "", nil, err
	}
	cleanup = func() { _ = os.RemoveAll(dir) }

	dbPath = filepath.Join(dir, "bench.db")
	st, err = store.Open(ctx, dbPath)
	if err != nil {
		cleanup()
		return nil, "", nil, err
	}
	prevCleanup := cleanup
	innerCleanup := func() { _ = st.Close(); prevCleanup() }
	cleanup = innerCleanup

	r := rand.New(rand.NewSource(42)) //nolint:gosec // synthetic bench data, crypto randomness not needed
	rows := make([]store.PendingChunk, n)
	for i := range n {
		v := syntheticVecR(dim, r)
		rows[i] = store.PendingChunk{
			Path:       filepath.Join("pkg", "file"+itoa(i)+".go"),
			Kind:       "function_declaration",
			ContentSHA: "sha" + itoa(i),
			Content:    "func F" + itoa(i) + "() { /* synthetic */ }",
			Vec:        v,
		}
	}
	if upsertErr := st.UpsertMany(ctx, rows, time.Now()); upsertErr != nil {
		cleanup()
		return nil, "", nil, upsertErr
	}
	return st, dbPath, cleanup, nil
}

func syntheticVec(dim int, seed int64) []float32 {
	r := rand.New(rand.NewSource(seed)) //nolint:gosec // synthetic bench data
	return syntheticVecR(dim, r)
}

func syntheticVecR(dim int, r *rand.Rand) []float32 {
	v := make([]float32, dim)
	for i := range v {
		v[i] = float32(r.NormFloat64())
	}
	return v
}

// timeN benchmarks fn with n iterations and returns percentile latencies.
func timeN(name string, n int, fn func() string) RunResult {
	samples := make([]time.Duration, n)
	for i := range n {
		t := time.Now()
		_ = fn()
		samples[i] = time.Since(t)
	}
	return RunResult{Name: name, Iterations: n, P50: pct(samples, 50), P95: pct(samples, 95), P99: pct(samples, 99)}
}

// timeNErr benchmarks an error-returning fn.
func timeNErr(name string, n int, fn func() error) RunResult {
	samples := make([]time.Duration, n)
	for i := range n {
		t := time.Now()
		_ = fn()
		samples[i] = time.Since(t)
	}
	return RunResult{Name: name, Iterations: n, P50: pct(samples, 50), P95: pct(samples, 95), P99: pct(samples, 99)}
}

func pct(samples []time.Duration, p int) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	sorted := make([]time.Duration, len(samples))
	copy(sorted, samples)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := (len(sorted) - 1) * p / 100
	return sorted[idx]
}

func nameKNN(n, dim int) string     { return "knn/" + itoa(n) + "x" + itoa(dim) }
func nameStorage(n, dim int) string { return "storage/" + itoa(n) + "x" + itoa(dim) }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	buf := make([]byte, 0, 12)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		buf = append(buf, byte('0'+n%10))
		n /= 10
	}
	if neg {
		buf = append(buf, '-')
	}
	for i, j := 0, len(buf)-1; i < j; i, j = i+1, j-1 {
		buf[i], buf[j] = buf[j], buf[i]
	}
	return string(buf)
}
