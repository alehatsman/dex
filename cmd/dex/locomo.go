package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/alehatsman/dex/internal/locomo"
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

func runBench(ctx context.Context, args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: dex bench <subcommand> [flags]\n\nSubcommands:\n  locomo  LoCoMo memory-recall benchmark")
		os.Exit(1)
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "locomo":
		runLocomo(ctx, rest)
	default:
		fmt.Fprintf(os.Stderr, "dex bench: unknown subcommand %q\n", sub)
		os.Exit(1)
	}
}

func runLocomo(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("bench locomo", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, locomoUsage) }

	datasetPath := fs.String("dataset", "", "NDJSON dataset file (default: bundled reference.ndjson)")
	k := fs.Int("k", 5, "retrieval depth")
	outputFmt := fs.String("output", "md", "output format: json or md")
	checkPath := fs.String("check", "", "reference JSON to check for regression")

	_ = fs.Parse(args)
	if fs.NArg() < 1 {
		fs.Usage()
		os.Exit(1)
	}
	projectPath := fs.Arg(0)

	// Resolve dataset path — default to the bundled reference alongside the binary.
	dsPath := *datasetPath
	if dsPath == "" {
		// Walk up from the binary to find benchmark/locomo/reference.ndjson.
		// In development the binary is in cmd/dex; in install it's in ~/bin.
		// Try the repo root relative to the source file first (dev), then fall
		// back to a path alongside the installed binary.
		_, srcFile, _, _ := runtime.Caller(0)
		repoRoot := filepath.Join(filepath.Dir(srcFile), "..", "..")
		candidate := filepath.Join(repoRoot, "benchmark", "locomo", "reference.ndjson")
		if _, err := os.Stat(candidate); err == nil {
			dsPath = candidate
		} else {
			// Installed: look next to the binary.
			exe, _ := os.Executable()
			dsPath = filepath.Join(filepath.Dir(exe), "..", "share", "dex", "locomo", "reference.ndjson")
		}
	}

	d, err := locomo.LoadFile(dsPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dex bench locomo: load dataset %q: %v\n", dsPath, err)
		os.Exit(1)
	}
	if len(d.Conversations) == 0 {
		fmt.Fprintln(os.Stderr, "dex bench locomo: dataset is empty")
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "dex bench locomo: %d conversations, %d turns, %d questions\n",
		len(d.Conversations), d.TotalTurns(), len(d.Questions()))

	// Open the project store just to get the recorded embed model.
	_ = projectPath // used by newEmbedClient indirectly via the store
	em := newEmbedClient("")

	results, err := locomo.Run(ctx, em, d, *k)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dex bench locomo: run: %v\n", err)
		os.Exit(1)
	}

	rep := locomo.Compute(results, *k)

	switch *outputFmt {
	case "json":
		out, err := rep.JSON()
		if err != nil {
			fmt.Fprintf(os.Stderr, "dex bench locomo: marshal: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(out))
	default:
		fmt.Print(rep.Markdown())
	}

	if *checkPath != "" {
		if err := checkRegression(rep, *checkPath); err != nil {
			fmt.Fprintf(os.Stderr, "dex bench locomo: regression check failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "dex bench locomo: regression check passed")
	}
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
