package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/alehatsman/dex/internal/eval/locomo"
)

const locomoUsage = `Usage: dex bench locomo <project-path> [flags]

Run the LoCoMo memory-recall benchmark against the dex embed endpoint.
Ingests each conversation turn as a memory, then retrieves top-k memories
per question and scores recall@k, token-F1, and exact-match.

Flags:
  --dataset path   NDJSON dataset file (default: bundled reference.ndjson)
  --k int          retrieval depth (default: 5)
  --output format  json or md (default: md)
  --check path     compare results against a reference JSON; exit 1 on regression

Environment: DEX_EMBED_URL, DEX_EMBED_MODEL, DEX_EMBED_BATCH — same as indexing.
`

func runBench(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintln(os.Stderr, "Usage: dex bench <subcommand> [flags]\n\nSubcommands:\n  locomo    LoCoMo memory-recall benchmark\n  eval      offline code-retrieval eval (this repo's git history)\n  corpus    multi-repo retrieval eval (pinned real repos)\n  compress  offline compression benchmark (ratio/anchor%/fidelity)\n  perf      local-compute pipeline performance benchmark\n  nav       navigation cost: calls+tokens to first gold-file touch\n  trace     cross-language trace-precision eval (callers/callees vs gold)\n  skew      cross-language centrality skew (pagerank share vs node share)\n  cochange  blast-radius co-change structural-coverage ceiling (graph reachability)")
		return flag.ErrHelp
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "locomo":
		return runLocomo(ctx, rest)
	case "eval":
		return runEval(ctx, rest)
	case "corpus":
		return runCorpus(ctx, rest)
	case "compress":
		return runBenchCompress(ctx, rest)
	case "perf":
		return runBenchPerf(ctx, rest)
	case "nav":
		return runBenchNav(ctx, rest)
	case "trace":
		return runBenchTrace(ctx, rest)
	case "skew":
		return runBenchSkew(ctx, rest)
	case "cochange":
		return runBenchCochange(ctx, rest)
	default:
		return fmt.Errorf("dex bench: unknown subcommand %q", sub)
	}
}

func runLocomo(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("bench locomo", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, locomoUsage) }

	datasetPath := fs.String("dataset", "", "NDJSON dataset file (default: bundled reference.ndjson)")
	k := fs.Int("k", 5, "retrieval depth")
	outputFmt := fs.String("output", "md", "output format: json or md")
	checkPath := fs.String("check", "", "reference JSON to check for regression")

	// Pull out the project path (first non-flag arg) before fs.Parse so that
	// flags after the path (e.g. "dex bench locomo . --output json") work.
	var projectPath string
	var flagArgs []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") || projectPath != "" {
			flagArgs = append(flagArgs, a)
		} else {
			projectPath = a
		}
	}
	if projectPath == "" {
		fs.Usage()
		return flag.ErrHelp
	}
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}

	// Resolve dataset path. Search order:
	// 1. --dataset flag (explicit)
	// 2. benchmark/locomo/reference.ndjson relative to <project-path> (repo root)
	// 3. benchmark/locomo/reference.ndjson relative to cwd
	dsPath := *datasetPath
	if dsPath == "" {
		for _, base := range []string{projectPath, "."} {
			c := filepath.Join(base, "benchmark", "locomo", "reference.ndjson")
			if _, err := os.Stat(c); err == nil {
				dsPath = c
				break
			}
		}
	}
	if dsPath == "" {
		return fmt.Errorf("dex bench locomo: dataset not found; pass --dataset <path>")
	}

	d, err := locomo.LoadFile(dsPath)
	if err != nil {
		return fmt.Errorf("dex bench locomo: load dataset %q: %w", dsPath, err)
	}
	if len(d.Conversations) == 0 {
		return fmt.Errorf("dex bench locomo: dataset is empty")
	}
	fmt.Fprintf(os.Stderr, "dex bench locomo: %d conversations, %d turns, %d questions\n",
		len(d.Conversations), d.TotalTurns(), len(d.Questions()))

	// Open the project store just to get the recorded embed model.
	_ = projectPath // used by newEmbedClient indirectly via the store
	em := newEmbedClient("")

	results, err := locomo.Run(ctx, em, d, *k)
	if err != nil {
		return fmt.Errorf("dex bench locomo: run: %w", err)
	}

	rep := locomo.Compute(results, *k)

	switch *outputFmt {
	case "json":
		out, err := rep.JSON()
		if err != nil {
			return fmt.Errorf("dex bench locomo: marshal: %w", err)
		}
		fmt.Println(string(out))
	default:
		fmt.Print(rep.Markdown())
	}

	if *checkPath != "" {
		if err := checkRegression(rep, *checkPath); err != nil {
			return fmt.Errorf("dex bench locomo: regression check failed: %w", err)
		}
		fmt.Fprintln(os.Stderr, "dex bench locomo: regression check passed")
	}
	return nil
}

// checkRegression loads a reference report from refPath and fails if the
// current overall recall@k or mean token-F1 is lower by more than 0.02.
func checkRegression(current locomo.Report, refPath string) error {
	data, err := os.ReadFile(refPath)
	if err != nil {
		return fmt.Errorf("read reference %q: %w", refPath, err)
	}
	var ref locomo.Report
	if err := json.Unmarshal(data, &ref); err != nil {
		return fmt.Errorf("parse reference: %w", err)
	}
	const tol = 0.02
	if d := ref.Overall.RecallAtK - current.Overall.RecallAtK; d > tol {
		return fmt.Errorf("recall@k regressed: was %.3f, now %.3f (delta %.3f > tol %.2f)",
			ref.Overall.RecallAtK, current.Overall.RecallAtK, d, tol)
	}
	if d := ref.Overall.MeanTokenF1 - current.Overall.MeanTokenF1; d > tol {
		return fmt.Errorf("mean token-F1 regressed: was %.3f, now %.3f (delta %.3f > tol %.2f)",
			ref.Overall.MeanTokenF1, current.Overall.MeanTokenF1, d, tol)
	}
	return nil
}
