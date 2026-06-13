package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/alehatsman/dex/internal/embed"
	"github.com/alehatsman/dex/internal/eval"
	"github.com/alehatsman/dex/internal/mcp"
	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/store"
)

const evalUsage = `Usage: dex bench eval <project-path> [flags]

Offline code-retrieval eval. Scores the live Search path against a golden set
of (query → relevant files) pairs mined from the repo's own git history, and
reports NDCG@k, Recall@k and MRR.

Four golden-set flavors (--mode):
  git-history  (default) query = commit subject, relevant = files it touched.
               Measures direct code retrieval.
  blast-radius query = a code excerpt from an anchor file, relevant = the OTHER
               files co-changed in the same commit (anchor excluded from
               results). Measures structural / "what changes with this?"
               retrieval — co-change probe for graph-lane work.
  structural   query = prose commit subject, relevant = co-changed files that
               span ≥2 packages with NO import relationship between them.
               Measures graph-lane contribution for structural coupling invisible
               to BM25+dense — the instrument for tuning DEX_GRAPH_WEIGHT (#279).
  orphan       query = natural-language phrase derived from a package-level
               import, const, or var declaration. Relevant = the file
               containing that declaration. Targets the "orphan" chunks
               that commit-history probes miss.

The golden set is committed (default benchmark/eval/golden.json, or
blast-radius.json / structural.json for the corresponding --mode) so the
query set — and therefore the metrics — are stable across runs. Regenerate
with --gen to refresh the labels.

Flags:
  --gen            (re)generate the golden set from git history and write it to
                   --golden, then exit (does not score)
  --mode flavor    golden-set flavor when generating: git-history | blast-radius | structural | orphan
  --golden path    golden-set JSON (default: <project>/benchmark/eval/golden.json)
  --k int          retrieval depth (default: 10)
  --max-commits N  commits to scan when generating (default: 500)
  --max-files N    skip commits touching more than N code files (default: 5)
  --max-per-kind N orphan mode: max queries per declaration kind (default: 50)
  --output format  json or md (default: md)
  --check path     compare against a reference report JSON; exit 1 on regression
  --alpha-sweep   sweep FusionLinear α from 0.1 to 1.0 in 0.1 steps, printing
                   a table with the RRF baseline. Use to tune DEX_FUSION_ALPHA.

Environment: DEX_EMBED_URL, DEX_EMBED_MODEL, DEX_EMBED_BATCH — same as indexing.
             DEX_FUSION_MODE=linear  select convex-combination score fusion.
             DEX_FUSION_ALPHA=0.5    dense weight for FusionLinear (0 < α ≤ 1).
`

func runEval(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("bench eval", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, evalUsage) }

	gen := fs.Bool("gen", false, "regenerate golden set from git history and exit")
	mode := fs.String("mode", "git-history", "golden-set flavor when generating: git-history | blast-radius | structural | orphan")
	goldenPath := fs.String("golden", "", "golden-set JSON path")
	k := fs.Int("k", 10, "retrieval depth")
	maxCommits := fs.Int("max-commits", 0, "commits to scan when generating")
	maxFiles := fs.Int("max-files", 0, "skip commits touching more than N code files")
	maxPerKind := fs.Int("max-per-kind", 0, "orphan mode: max queries per declaration kind (default: 50)")
	outputFmt := fs.String("output", "md", "output format: json or md")
	checkPath := fs.String("check", "", "reference report JSON to check for regression")
	lane := fs.String("lane", "full", "retrieval lane: full (semantic+BM25, default) | bm25 (BM25+symbol+graph, zero-inference) | onnx (in-process ONNX, requires env vars)")
	alphaSweep := fs.Bool("alpha-sweep", false, "sweep FusionLinear α from 0.1 to 1.0 and print a comparison table with RRF as baseline")
	expand := fs.String("expand", "off", "query-side expansion to A/B (#252): off | on | full. Requires DEX_EXPAND_MODEL.")

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
		return flag.ErrHelp
	}
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}

	p, err := resolveEvalProject(projectPath)
	if err != nil {
		return fmt.Errorf("dex bench eval: %w", err)
	}

	validModes := map[string]bool{"git-history": true, "blast-radius": true, "structural": true, "orphan": true}
	if !validModes[*mode] {
		return fmt.Errorf("dex bench eval: unknown --mode %q (want git-history|blast-radius|structural|orphan)", *mode)
	}

	gPath := *goldenPath
	if gPath == "" {
		name := "golden.json"
		switch *mode {
		case "blast-radius":
			name = "blast-radius.json"
		case "structural":
			name = "structural.json"
		case "orphan":
			name = "orphan.json"
		}
		gPath = filepath.Join(p.Root, "benchmark", "eval", name)
	}

	if *gen {
		opts := eval.GenOpts{MaxCommits: *maxCommits, MaxFiles: *maxFiles}
		var gs eval.GoldenSet
		var err error
		switch *mode {
		case "blast-radius":
			gs, err = eval.GenerateBlastRadius(ctx, p.Root, opts)
		case "structural":
			gs, err = eval.GenerateStructural(ctx, p.Root, opts)
		case "orphan":
			var ocounts eval.OrphanGenCounts
			gs, ocounts, err = eval.GenerateOrphan(ctx, p.Root, eval.OrphanOpts{MaxFiles: *maxFiles, MaxPerKind: *maxPerKind})
			if err == nil {
				fmt.Fprintf(os.Stderr, "dex bench eval: orphan gen: imports=%d consts=%d vars=%d\n",
					ocounts.Imports, ocounts.Consts, ocounts.Vars)
			}
		default:
			gs, err = eval.Generate(ctx, p.Root, opts)
		}
		if err != nil {
			return fmt.Errorf("dex bench eval: generate: %w", err)
		}
		if err := gs.Save(gPath); err != nil {
			return fmt.Errorf("dex bench eval: save golden set: %w", err)
		}
		fmt.Fprintf(os.Stderr, "dex bench eval: wrote %d queries to %s (head %s)\n",
			len(gs.Queries), gPath, shortHash(gs.Head))
		return nil
	}

	gs, err := eval.LoadGolden(gPath)
	if err != nil {
		return fmt.Errorf("dex bench eval: load golden set: %w\n  (generate one with: dex bench eval %s --gen)", err, projectPath)
	}
	if len(gs.Queries) == 0 {
		return fmt.Errorf("dex bench eval: golden set is empty")
	}

	if _, err := os.Stat(p.DBPath); err != nil {
		return fmt.Errorf("dex bench eval: no index for %s — run `dex index %s` first", p.Root, p.Root)
	}
	st, err := store.OpenWith(ctx, p.DBPath, storeOpts())
	if err != nil {
		return fmt.Errorf("dex bench eval: open store: %w", err)
	}
	defer func() { _ = st.Close() }()

	// Use the index-recorded embed model so the query vectors match the
	// indexed chunk vectors (dimension + semantics).
	stats, _ := st.Stats(ctx)
	em, err := evalEmbedForLane(*lane, stats.EmbedModel)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "dex bench eval: %d queries, k=%d, index %s\n", len(gs.Queries), *k, p.DBPath)

	if *alphaSweep {
		if err := runAlphaSweep(ctx, p, em, gs, *k); err != nil {
			return fmt.Errorf("dex bench eval: alpha sweep: %w", err)
		}
		return nil
	}

	var rw eval.Rewrite
	if *expand != "" && *expand != "off" {
		xc := newExpandClient(newChatClient())
		if xc == nil {
			fmt.Fprintln(os.Stderr, "dex bench eval: --expand set but DEX_EXPAND_MODEL is empty; expansion disabled")
		} else {
			mode := *expand
			rw = func(ctx context.Context, q string) (string, string) {
				return mcp.ExpandForEval(ctx, xc, mode, q)
			}
			fmt.Fprintf(os.Stderr, "dex bench eval: query expansion=%s via %s\n", mode, xc.ModelName())
		}
	}
	results, err := eval.RunWithRewrite(ctx, em, st, gs, *k, rw)
	if err != nil {
		return fmt.Errorf("dex bench eval: run: %w", err)
	}
	rep := eval.Compute(results, *k)

	switch *outputFmt {
	case "json":
		out, err := rep.JSON()
		if err != nil {
			return fmt.Errorf("dex bench eval: marshal: %w", err)
		}
		fmt.Println(string(out))
	default:
		fmt.Print(rep.Markdown())
	}

	if *checkPath != "" {
		if err := checkEvalRegression(rep, *checkPath); err != nil {
			return fmt.Errorf("dex bench eval: regression check failed: %w", err)
		}
		fmt.Fprintln(os.Stderr, "dex bench eval: regression check passed")
	}
	return nil
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

// evalEmbedForLane constructs the embedder for the given lane. Returns an
// error for unsupported configurations.
func evalEmbedForLane(lane, model string) (embed.Embedder, error) {
	switch strings.ToLower(lane) {
	case "bm25":
		fmt.Fprintln(os.Stderr, "dex bench eval: --lane bm25 — BM25+symbol+graph only (zero-inference)")
		return nil, nil
	case "onnx":
		if os.Getenv("DEX_ONNX_MODEL") == "" || os.Getenv("DEX_ONNXRUNTIME_LIB") == "" {
			fmt.Fprintln(os.Stderr, "dex bench eval: --lane onnx skipped — DEX_ONNX_MODEL/DEX_ONNXRUNTIME_LIB not set")
			return nil, flag.ErrHelp // exit 0 — not an error, just skip
		}
		_ = os.Setenv("DEX_EMBED_ENGINE", "onnx")
		return newEmbedClient(""), nil
	case "full", "":
		return newEmbedClient(model), nil
	default:
		return nil, fmt.Errorf("dex bench eval: unknown --lane %q (want full|bm25|onnx)", lane)
	}
}

// runAlphaSweep opens the store once per configuration (RRF baseline + FusionLinear
// at α = 0.1, 0.2, …, 1.0) and prints a comparison table so the operator can pick
// the best FusionAlpha value before setting DEX_FUSION_MODE=linear in production.
func runAlphaSweep(ctx context.Context, p *proj.Project, em embed.Embedder, gs eval.GoldenSet, k int) error {
	type row struct {
		label string
		rep   eval.Report
	}

	run := func(label string, opts store.Options) (row, error) {
		st, err := store.OpenWith(ctx, p.DBPath, opts)
		if err != nil {
			return row{}, fmt.Errorf("%s: %w", label, err)
		}
		defer func() { _ = st.Close() }()
		results, err := eval.Run(ctx, em, st, gs, k)
		if err != nil {
			return row{}, fmt.Errorf("%s: %w", label, err)
		}
		return row{label: label, rep: eval.Compute(results, k)}, nil
	}

	base := storeOpts()
	base.FusionMode = store.FusionRRF

	rows := make([]row, 0, 12)
	r, err := run("rrf (baseline)", base)
	if err != nil {
		return err
	}
	rows = append(rows, r)

	fmt.Fprintf(os.Stderr, "alpha sweep: rrf done; running linear α 0.1…1.0\n")
	for i := 1; i <= 10; i++ {
		alpha := float32(i) / 10.0
		opts := storeOpts()
		opts.FusionMode = store.FusionLinear
		opts.FusionAlpha = alpha
		label := fmt.Sprintf("linear α=%.1f", alpha)
		r, err := run(label, opts)
		if err != nil {
			return err
		}
		rows = append(rows, r)
		fmt.Fprintf(os.Stderr, "alpha sweep: %s done\n", label)
	}

	// Print comparison table.
	fmt.Printf("\n%-20s  %8s  %8s  %8s\n", "mode", "NDCG@k", "Recall@k", "MRR")
	fmt.Printf("%-20s  %8s  %8s  %8s\n", "--------------------", "--------", "--------", "--------")
	for _, row := range rows {
		fmt.Printf("%-20s  %8.4f  %8.4f  %8.4f\n",
			row.label, row.rep.MeanNDCG, row.rep.MeanRecall, row.rep.MRR)
	}
	return nil
}
