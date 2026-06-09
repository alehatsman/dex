package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/alehatsman/dex/internal/eval"
	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/store"
)

const evalUsage = `Usage: dex bench eval <project-path> [flags]

Offline code-retrieval eval. Scores the live Search path against a golden set
of (query → relevant files) pairs mined from the repo's own git history, and
reports NDCG@k, Recall@k and MRR.

The golden set is committed (default benchmark/eval/golden.json) so the query
set — and therefore the metrics — are stable across runs. Regenerate it from
git history with --gen when you want to refresh the labels.

Flags:
  --gen            (re)generate the golden set from git history and write it to
                   --golden, then exit (does not score)
  --golden path    golden-set JSON (default: <project>/benchmark/eval/golden.json)
  --k int          retrieval depth (default: 10)
  --max-commits N  commits to scan when generating (default: 500)
  --max-files N    skip commits touching more than N code files (default: 5)
  --output format  json or md (default: md)
  --check path     compare against a reference report JSON; exit 1 on regression

Environment: DEX_EMBED_URL, DEX_EMBED_MODEL, DEX_EMBED_BATCH — same as indexing.
`

func runEval(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("bench eval", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, evalUsage) }

	gen := fs.Bool("gen", false, "regenerate golden set from git history and exit")
	goldenPath := fs.String("golden", "", "golden-set JSON path")
	k := fs.Int("k", 10, "retrieval depth")
	maxCommits := fs.Int("max-commits", 0, "commits to scan when generating")
	maxFiles := fs.Int("max-files", 0, "skip commits touching more than N code files")
	outputFmt := fs.String("output", "md", "output format: json or md")
	checkPath := fs.String("check", "", "reference report JSON to check for regression")

	// Project path is the first non-flag arg; allow flags after it.
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
		os.Exit(1)
	}
	_ = fs.Parse(flagArgs)

	p, err := resolveEvalProject(projectPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dex bench eval: %v\n", err)
		os.Exit(1)
	}

	gPath := *goldenPath
	if gPath == "" {
		gPath = filepath.Join(p.Root, "benchmark", "eval", "golden.json")
	}

	if *gen {
		gs, err := eval.Generate(ctx, p.Root, eval.GenOpts{MaxCommits: *maxCommits, MaxFiles: *maxFiles})
		if err != nil {
			fmt.Fprintf(os.Stderr, "dex bench eval: generate: %v\n", err)
			os.Exit(1)
		}
		if err := gs.Save(gPath); err != nil {
			fmt.Fprintf(os.Stderr, "dex bench eval: save golden set: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "dex bench eval: wrote %d queries to %s (head %s)\n",
			len(gs.Queries), gPath, shortHash(gs.Head))
		return
	}

	gs, err := eval.LoadGolden(gPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dex bench eval: load golden set: %v\n  (generate one with: dex bench eval %s --gen)\n", err, projectPath)
		os.Exit(1)
	}
	if len(gs.Queries) == 0 {
		fmt.Fprintln(os.Stderr, "dex bench eval: golden set is empty")
		os.Exit(1)
	}

	if _, err := os.Stat(p.DBPath); err != nil {
		fmt.Fprintf(os.Stderr, "dex bench eval: no index for %s — run `dex index %s` first\n", p.Root, p.Root)
		os.Exit(1)
	}
	st, err := store.OpenWith(ctx, p.DBPath, storeOpts())
	if err != nil {
		fmt.Fprintf(os.Stderr, "dex bench eval: open store: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = st.Close() }()

	// Use the index-recorded embed model so the query vectors match the
	// indexed chunk vectors (dimension + semantics).
	stats, _ := st.Stats(ctx)
	em := newEmbedClient(stats.EmbedModel)

	fmt.Fprintf(os.Stderr, "dex bench eval: %d queries, k=%d, index %s\n", len(gs.Queries), *k, p.DBPath)

	results, err := eval.Run(ctx, em, st, gs, *k)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dex bench eval: run: %v\n", err)
		os.Exit(1)
	}
	rep := eval.Compute(results, *k)

	switch *outputFmt {
	case "json":
		out, err := rep.JSON()
		if err != nil {
			fmt.Fprintf(os.Stderr, "dex bench eval: marshal: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(out))
	default:
		fmt.Print(rep.Markdown())
	}

	if *checkPath != "" {
		if err := checkEvalRegression(rep, *checkPath); err != nil {
			fmt.Fprintf(os.Stderr, "dex bench eval: regression check failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "dex bench eval: regression check passed")
	}
}

// resolveEvalProject resolves the project path to its index identity.
func resolveEvalProject(path string) (*proj.Project, error) {
	base, err := indexDir()
	if err != nil {
		return nil, err
	}
	return proj.Resolve(path, base)
}

// checkEvalRegression fails if NDCG@k, Recall@k or MRR dropped by more than
// the tolerance versus a committed reference report.
func checkEvalRegression(current eval.Report, refPath string) error {
	data, err := os.ReadFile(refPath)
	if err != nil {
		return fmt.Errorf("read reference %q: %w", refPath, err)
	}
	var ref eval.Report
	if err := json.Unmarshal(data, &ref); err != nil {
		return fmt.Errorf("parse reference: %w", err)
	}
	const tol = 0.02
	regs := current.Regressions(ref, tol)
	if len(regs) == 0 {
		return nil
	}
	msgs := make([]string, len(regs))
	for i, r := range regs {
		msgs[i] = r.String()
	}
	return fmt.Errorf("%s (tol %.2f)", strings.Join(msgs, "; "), tol)
}

func shortHash(h string) string {
	if len(h) > 8 {
		return h[:8]
	}
	return h
}
