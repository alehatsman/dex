package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/alehatsman/dex/internal/benchcompress"
	"github.com/alehatsman/dex/internal/tokens"
)

const benchCompressUsage = `Usage: dex bench compress [flags]

Offline deterministic compression benchmark. Measures ratio, anchor
preservation, extractive fidelity, and round-trip correctness per pass.
Runs with zero inference — no embed or chat calls.

Two gate classes:
  lossless passes (codebook, ngram_codebook, symmap): gated on ratio +
    round-trip == original (hard fail on reconstruction error).
  lossy passes (aggressive, entropy, terse, ib): gated on ratio +
    extractive-fidelity floor.

Flags:
  --tokenizer name  tokenizer family: o200k_base | cl100k_base | llama
                    (default: o200k_base; matches lean-ctx COUNTING_FAMILY)
  --output format   json or md (default: md)
  --check path      compare against baseline JSON; exit 1 on regression
                    (default: benchmark/compress/baseline.json when present)

What it does NOT measure:
  - embedding-similarity fidelity (--with-embed, deferred; needs GPU)
  - task-success-per-token (--with-llm, deferred; the #157 north star)
  - compression of content types not represented in the built-in corpus
`

func runBenchCompress(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("bench compress", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, benchCompressUsage) }

	tokenizerName := fs.String("tokenizer", "o200k_base", "tokenizer family: o200k_base|cl100k_base|llama")
	outputFmt := fs.String("output", "md", "output format: json or md")
	checkPath := fs.String("check", "", "baseline JSON for regression check")

	if err := fs.Parse(args); err != nil {
		return err
	}

	family := tokens.Detect(*tokenizerName)
	// Align the package-level accounting + token-reduction gating (#292) with
	// the requested tokenizer, so the aggressive pass exercises rule
	// eligibility under this encoder — the per-tokenizer Pareto the bench
	// reports. Counting elsewhere uses the explicit family passed to RunSample.
	tokens.SetDefaultFamily(family)

	// Run all samples.
	var sampleResults []benchcompress.SampleResult
	for _, s := range benchcompress.BuiltinCorpus {
		sr := benchcompress.RunSample(s, family)
		sampleResults = append(sampleResults, sr)
	}

	rep := benchcompress.Aggregate(sampleResults, family.String())

	switch *outputFmt {
	case "json":
		out, err := rep.JSON()
		if err != nil {
			return fmt.Errorf("dex bench compress: marshal: %w", err)
		}
		fmt.Println(string(out))
	default:
		fmt.Print(rep.Markdown())
	}

	// Resolve --check default.
	cp := *checkPath
	if cp == "" {
		candidate := filepath.Join("benchmark", "compress", "baseline.json")
		if _, err := os.Stat(candidate); err == nil {
			cp = candidate
		}
	}
	if cp != "" {
		if err := benchcompress.CheckRegression(rep, cp); err != nil {
			return fmt.Errorf("dex bench compress: regression check failed: %w", err)
		}
		fmt.Fprintln(os.Stderr, "dex bench compress: regression check passed")
	}
	return nil
}
