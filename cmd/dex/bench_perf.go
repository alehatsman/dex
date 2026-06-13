package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/alehatsman/dex/internal/benchperf"
)

const benchPerfUsage = `Usage: dex bench perf [flags]

Offline local-compute performance benchmark. Measures compress-pass latency,
KNN vector search scaling curves, BM25/FTS5 search latency, and index storage
footprint. All timed paths use synthetic data — zero GPU, zero network.

KNN scaling curves show where brute-force KNN exceeds a latency budget,
identifying the break-even point for the ANN index (#216).

Storage numbers justify int8 quantisation (#215) and Matryoshka dims (#249).

Flags:
  --iters int       timed repetitions per target (default: 100)
  --dim int         vector dimensionality for synthetic data (default: 1024)
  --output format   json or md (default: md)
  --check path      compare against baseline JSON; exit 1 on regression
                    (default: benchmark/perf/baseline.json when present)

Report-only (NOT gated — GPU/network-bound, wildly variable):
  embed/rerank RTT, ask end-to-end, indexing throughput.

What it does NOT measure:
  - embedding throughput (GPU-bound, variable; report-only, deferred)
  - rerank/chat RTT (network-bound; report-only, deferred)
  - graph expansion latency (no synthetic graph builder yet; deferred)
  - cold startup latency (requires a full dex binary invocation; use 'time dex ask')
`

func runBenchPerf(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("bench perf", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, benchPerfUsage) }

	iters := fs.Int("iters", 100, "timed repetitions per target")
	dim := fs.Int("dim", 1024, "vector dimensionality")
	outputFmt := fs.String("output", "md", "output format: json or md")
	checkPath := fs.String("check", "", "baseline JSON for regression check")

	if err := fs.Parse(args); err != nil {
		return err
	}

	opts := benchperf.Opts{
		Iterations: *iters,
		Dim:        *dim,
	}

	fmt.Fprintln(os.Stderr, "dex bench perf: running local-compute suite (no GPU/network)...")
	results, err := benchperf.Run(opts)
	if err != nil {
		return fmt.Errorf("dex bench perf: %w", err)
	}

	rep := benchperf.Report{Dim: *dim, Results: results}

	switch *outputFmt {
	case "json":
		out, err := rep.JSON()
		if err != nil {
			return fmt.Errorf("dex bench perf: marshal: %w", err)
		}
		fmt.Println(string(out))
	default:
		fmt.Print(rep.Markdown())
	}

	// Resolve --check default.
	cp := *checkPath
	if cp == "" {
		candidate := filepath.Join("benchmark", "perf", "baseline.json")
		if _, err := os.Stat(candidate); err == nil {
			cp = candidate
		}
	}
	if cp != "" {
		if err := benchperf.CheckRegression(rep, cp); err != nil {
			return fmt.Errorf("dex bench perf: regression check failed: %w", err)
		}
		fmt.Fprintln(os.Stderr, "dex bench perf: regression check passed")
	}
	return nil
}
