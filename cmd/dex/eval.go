package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alehatsman/dex/internal/embed"
	"github.com/alehatsman/dex/internal/eval"
	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/retrieve"
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
  --check path     compare against a reference report JSON; exit 1 on regression.
                   Fails closed when the reference's experiment manifest
                   (lane/model/mode/k/fusion/query-corpus) is incompatible, and
                   on a per-query-type bucket regression — not only the
                   aggregate. Both HEADs and the manifest are recorded in the
                   report so runs are only ever compared like-for-like.
  --allow-incompatible  with --check: compare despite an incompatible manifest
  --allow-stale-golden  compare even when the golden HEAD != current repo HEAD
  --alpha-sweep   sweep FusionLinear α from 0.1 to 1.0 in 0.1 steps, printing
                   a table with the RRF baseline. Use to tune DEX_FUSION_ALPHA.
  --graph-sweep   sweep GraphLaneWeight (off, 0.5…2.0) at the calibrated fusion
                   default; prints per-weight ΔNDCG/ΔRecall vs graph-off (#470).
                   Run with --mode structural/blast-radius for a real signal.
  --emit-calibration path  with --alpha-sweep: write the winning config to the
                   named calibration.yml (run from the dex repo, commit the diff)

Environment: DEX_EMBED_URL, DEX_EMBED_MODEL, DEX_EMBED_BATCH — same as indexing.
             DEX_FUSION_MODE=linear  select convex-combination score fusion.
             DEX_FUSION_ALPHA=0.5    dense weight for FusionLinear (0 < α ≤ 1).
`

// splitEvalArgs separates the project path (first non-flag arg) from flag args,
// allowing flags to appear after the positional argument.
func splitEvalArgs(args []string) (projectPath string, flagArgs []string) {
	for _, a := range args {
		if strings.HasPrefix(a, "-") || projectPath != "" {
			flagArgs = append(flagArgs, a)
		} else {
			projectPath = a
		}
	}
	return
}

// goldenPathForMode returns the default golden-set path for a generation mode
// when no explicit --golden path was given.
func goldenPathForMode(root, mode string) string {
	name := "golden.json"
	switch mode {
	case "blast-radius":
		name = "blast-radius.json"
	case "structural":
		name = "structural.json"
	case "orphan":
		name = "orphan.json"
	}
	return filepath.Join(root, "benchmark", "eval", name)
}

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
	lane := fs.String("lane", "full", "retrieval lane: full (semantic+BM25, default) | bm25 (BM25+graph store lane; no router/ask exact-symbol; zero-inference) | onnx (in-process ONNX, requires env vars)")
	alphaSweep := fs.Bool("alpha-sweep", false, "sweep FusionLinear α from 0.1 to 1.0 and print a comparison table with RRF as baseline")
	graphSweep := fs.Bool("graph-sweep", false, "sweep GraphLaneWeight (off, 0.5…2.0) at the calibrated fusion default and print per-weight NDCG/Recall deltas vs graph-off — the GraphLaneWeight ablation (#470)")
	emitCalib := fs.String("emit-calibration", "", "with --alpha-sweep: write the winning config to this calibration.yml path (run from the dex repo; commit the diff)")
	expand := fs.String("expand", "off", "query-side expansion to A/B (#252): off | on | full. Requires DEX_EXPAND_MODEL.")
	faithfulness := fs.Bool("faithfulness", false, "answer-faithfulness gate (#550): synthesize an ask answer per query and score how well it is grounded in the retrieved evidence. Requires a chat model (DEX_CHAT_URL/DEX_CHAT_MODEL).")
	allowStale := fs.Bool("allow-stale-golden", false, "compare even when the golden set's HEAD differs from the current repo HEAD (deliberate historical comparison). Without it, --check fails on a stale golden set.")
	allowIncompat := fs.Bool("allow-incompatible", false, "with --check: compare even when the reference report's experiment manifest (lane/model/mode/k/fusion/query-corpus) is incompatible. Without it, the check fails closed.")

	// Project path is the first non-flag arg; allow flags after it.
	projectPath, flagArgs := splitEvalArgs(args)
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
		gPath = goldenPathForMode(p.Root, *mode)
	}

	if *gen {
		return generateEvalGolden(ctx, p.Root, gPath, *mode, *maxCommits, *maxFiles, *maxPerKind)
	}

	gs, err := eval.LoadGolden(gPath)
	if err != nil {
		return fmt.Errorf("dex bench eval: load golden set: %w\n  (generate one with: dex bench eval %s --gen)", err, projectPath)
	}
	gs, valIssues := eval.ValidateGolden(gs)
	for _, iss := range valIssues {
		fmt.Fprintf(os.Stderr, "dex bench eval: warning: golden set: %s\n", iss)
	}
	if len(gs.Queries) == 0 {
		return fmt.Errorf("dex bench eval: golden set is empty (0 valid queries)")
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

	// Resolve the current repo HEAD and flag a stale golden set (generated
	// against a different HEAD). Warn always; fail under --check unless the
	// caller opts into a deliberate historical comparison.
	repoHead, err := checkStaleGolden(ctx, p.Root, gs, *checkPath != "", *allowStale)
	if err != nil {
		return err
	}

	if *alphaSweep && *graphSweep {
		return fmt.Errorf("dex bench eval: --alpha-sweep and --graph-sweep are mutually exclusive")
	}
	if *alphaSweep {
		if err := runAlphaSweep(ctx, p, em, gs, *k, *emitCalib); err != nil {
			return fmt.Errorf("dex bench eval: alpha sweep: %w", err)
		}
		return nil
	}
	if *graphSweep {
		if *emitCalib != "" {
			return fmt.Errorf("dex bench eval: --emit-calibration is not supported with --graph-sweep (graph-lane removal is a separate decision, see #470)")
		}
		if err := runGraphSweep(ctx, p, em, gs, *k); err != nil {
			return fmt.Errorf("dex bench eval: graph sweep: %w", err)
		}
		return nil
	}
	if *emitCalib != "" {
		return fmt.Errorf("dex bench eval: --emit-calibration requires --alpha-sweep")
	}
	if *faithfulness {
		if err := runFaithfulnessEval(ctx, st, em, gs, *k, *outputFmt, *checkPath); err != nil {
			return fmt.Errorf("dex bench eval: faithfulness: %w", err)
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
				return retrieve.ExpandForEval(ctx, xc, mode, q)
			}
			fmt.Fprintf(os.Stderr, "dex bench eval: query expansion=%s via %s\n", mode, xc.ModelName())
		}
	}
	results, err := eval.RunWithRewrite(ctx, em, st, gs, *k, rw)
	if err != nil {
		return fmt.Errorf("dex bench eval: run: %w", err)
	}
	rep := eval.Compute(results, *k)
	rep.Manifest = buildEvalManifest(*mode, gPath, gs, repoHead, *lane, *k, stats, storeOpts())

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
		if err := checkEvalRegression(rep, *checkPath, *allowIncompat); err != nil {
			return fmt.Errorf("dex bench eval: regression check failed: %w", err)
		}
		fmt.Fprintln(os.Stderr, "dex bench eval: regression check passed")
	}
	return nil
}

// checkStaleGolden resolves the repo HEAD and reports whether the golden set
// is stale (generated against a different HEAD). It warns always; when a
// regression check is active it returns an error unless the caller opted into
// a deliberate historical comparison. The resolved HEAD is returned for the
// report manifest regardless.
func checkStaleGolden(ctx context.Context, root string, gs eval.GoldenSet, checkActive, allowStale bool) (string, error) {
	repoHead, err := eval.RepoHead(ctx, root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dex bench eval: warning: could not resolve repo HEAD: %v\n", err)
	}
	if eval.StaleGolden(gs.Head, repoHead) {
		msg := fmt.Sprintf("golden set HEAD %s != repo HEAD %s — labels may be stale", shortHash(gs.Head), shortHash(repoHead))
		if checkActive && !allowStale {
			return repoHead, fmt.Errorf("dex bench eval: %s; regenerate with --gen or pass --allow-stale-golden", msg)
		}
		fmt.Fprintf(os.Stderr, "dex bench eval: warning: %s\n", msg)
	}
	return repoHead, nil
}

// buildEvalManifest stamps the experiment identity onto a report: the golden
// corpus (mode, file hash, query hash, generation HEAD), the repo HEAD at
// scoring time, and the retrieval configuration (lane, embed model/dim, fusion
// mode/alpha, graph weight, k). It is what lets --check refuse to compare
// runs produced under different conditions.
func buildEvalManifest(mode, goldenPath string, gs eval.GoldenSet, repoHead, lane string, k int, stats store.Stats, opts store.Options) *eval.EvalManifest {
	var goldenSHA string
	if data, err := os.ReadFile(goldenPath); err == nil {
		goldenSHA = eval.SHA256Hex(data)
	}
	return &eval.EvalManifest{
		SchemaVersion:  eval.ManifestSchemaVersion,
		GoldenMode:     mode,
		GoldenSHA256:   goldenSHA,
		GoldenHead:     gs.Head,
		RepoHead:       repoHead,
		QuerySetSHA256: eval.QuerySetSHA256(gs),
		Lane:           strings.ToLower(lane),
		EmbedModel:     stats.EmbedModel,
		EmbedDim:       stats.Dim,
		FusionMode:     store.FusionModeString(opts.FusionMode),
		FusionAlpha:    opts.FusionAlpha,
		GraphWeight:    opts.GraphLaneWeight,
		K:              k,
		RerankEnabled:  opts.Rerank != nil,
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

// checkEvalRegression fails if NDCG@k, Recall@k or MRR — globally or in any
// matching per-type bucket — dropped by more than the tolerance versus a
// committed reference report. It first gates on experiment-manifest
// compatibility: an incompatible reference (different lane/model/mode/k/fusion
// or query corpus) fails closed unless allowIncompat is set, so the metric
// comparison is only ever run on genuinely comparable numbers.
func checkEvalRegression(current eval.Report, refPath string, allowIncompat bool) error {
	data, err := os.ReadFile(refPath)
	if err != nil {
		return fmt.Errorf("read reference %q: %w", refPath, err)
	}
	var ref eval.Report
	if err := json.Unmarshal(data, &ref); err != nil {
		return fmt.Errorf("parse reference: %w", err)
	}

	// Manifest gate. Both sides must carry a manifest to compare identity;
	// a reference written before manifests existed falls back to a metric-only
	// comparison with a warning (regenerate the baseline to enable the gate).
	switch {
	case current.Manifest != nil && ref.Manifest != nil:
		if diffs := current.Manifest.Incompatible(*ref.Manifest); len(diffs) > 0 {
			if !allowIncompat {
				return fmt.Errorf("incompatible experiment manifest: %s — pass --allow-incompatible to compare anyway", strings.Join(diffs, ", "))
			}
			fmt.Fprintf(os.Stderr, "dex bench eval: warning: comparing incompatible runs: %s\n", strings.Join(diffs, ", "))
		}
	case ref.Manifest == nil:
		fmt.Fprintln(os.Stderr, "dex bench eval: warning: reference report predates experiment manifests — comparing metrics only; regenerate the baseline to enable the compatibility gate")
	}

	const (
		tol       = 0.02
		minBucket = 5
	)
	regs := current.Regressions(ref, tol)
	byType, bucketDelta := current.ByTypeRegressions(ref, tol, minBucket)
	regs = append(regs, byType...)
	for _, d := range bucketDelta {
		fmt.Fprintf(os.Stderr, "dex bench eval: warning: query-type bucket %s\n", d)
	}
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
		fmt.Fprintln(os.Stderr, "dex bench eval: --lane bm25 — BM25+graph store lane, no router/ask exact-symbol (zero-inference)")
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
func runAlphaSweep(ctx context.Context, p *proj.Project, em embed.Embedder, gs eval.GoldenSet, k int, emitPath string) error {
	type row struct {
		label string
		mode  store.FusionMode
		alpha float32
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
		return row{label: label, mode: opts.FusionMode, alpha: opts.FusionAlpha, rep: eval.Compute(results, k)}, nil
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

	// Winner = highest mean NDCG@k (ties keep the earlier, simpler config).
	best := 0
	for i, row := range rows {
		if row.rep.MeanNDCG > rows[best].rep.MeanNDCG {
			best = i
		}
	}

	// Print comparison table; mark the winner.
	fmt.Printf("\n%-20s  %8s  %8s  %8s\n", "mode", "NDCG@k", "Recall@k", "MRR")
	fmt.Printf("%-20s  %8s  %8s  %8s\n", "--------------------", "--------", "--------", "--------")
	for i, row := range rows {
		marker := ""
		if i == best {
			marker = "  <- winner"
		}
		fmt.Printf("%-20s  %8.4f  %8.4f  %8.4f%s\n",
			row.label, row.rep.MeanNDCG, row.rep.MeanRecall, row.rep.MRR, marker)
	}

	if emitPath == "" {
		return nil
	}
	return emitCalibration(emitPath, rows[best].mode, rows[best].alpha,
		fmt.Sprintf("alpha-sweep winner %q: NDCG@%d=%.4f Recall@%d=%.4f MRR=%.4f over %d queries (head %s)",
			rows[best].label, k, rows[best].rep.MeanNDCG, k, rows[best].rep.MeanRecall, rows[best].rep.MRR,
			len(gs.Queries), shortHash(gs.Head)))
}

// runGraphSweep measures the graph-proximity lane's marginal contribution: it
// opens the store with the lane held out (graph-off baseline) and then at a
// range of GraphLaneWeight values, all at the calibrated fusion default, and
// prints NDCG/Recall/MRR with ΔNDCG/ΔRecall vs the graph-off baseline. This is
// the GraphLaneWeight ablation (#470): if every weight row sits within ε of the
// baseline, the lane is inert at the current rerank pool and the weight knob is
// a no-op worth removing (a separate decision). The graph lane is exercised
// best by structural / blast-radius golden sets, so run with the matching
// --mode for a meaningful signal.
func runGraphSweep(ctx context.Context, p *proj.Project, em embed.Embedder, gs eval.GoldenSet, k int) error {
	type row struct {
		label  string
		weight float32 // 0 marks the graph-off baseline
		rep    eval.Report
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
		return row{label: label, weight: opts.GraphLaneWeight, rep: eval.Compute(results, k)}, nil
	}

	// Baseline: graph lane held out entirely (the true zero the weight can't express).
	off := storeOpts()
	off.GraphLaneDisabled = true
	baseline, err := run("off (baseline)", off)
	if err != nil {
		return err
	}
	rows := []row{baseline}

	calibrated := store.CalibratedDefaults().GraphLaneWeight
	weights := []float32{0.5, 1.0, 1.5, 2.0}
	fmt.Fprintf(os.Stderr, "graph sweep: off done; running weights %v at the calibrated fusion default\n", weights)
	for _, w := range weights {
		opts := storeOpts()
		opts.GraphLaneDisabled = false
		opts.GraphLaneWeight = w
		label := fmt.Sprintf("weight=%.1f", w)
		if w == calibrated {
			label += " (default)"
		}
		r, err := run(label, opts)
		if err != nil {
			return err
		}
		rows = append(rows, r)
		fmt.Fprintf(os.Stderr, "graph sweep: %s done\n", label)
	}

	// Winner = highest mean NDCG@k among the weighted rows (skip the baseline).
	best := 1
	for i := 1; i < len(rows); i++ {
		if rows[i].rep.MeanNDCG > rows[best].rep.MeanNDCG {
			best = i
		}
	}

	// Print comparison table with deltas vs the graph-off baseline.
	base := baseline.rep
	fmt.Printf("\n%-20s  %8s  %8s  %8s  %8s  %8s\n", "graph lane", "NDCG@k", "Recall@k", "MRR", "ΔNDCG", "ΔRecall")
	fmt.Printf("%-20s  %8s  %8s  %8s  %8s  %8s\n",
		"--------------------", "--------", "--------", "--------", "--------", "--------")
	for i, r := range rows {
		marker := ""
		if i == best {
			marker = "  <- winner"
		}
		if i == 0 { // baseline row: no delta against itself
			fmt.Printf("%-20s  %8.4f  %8.4f  %8.4f  %8s  %8s\n",
				r.label, r.rep.MeanNDCG, r.rep.MeanRecall, r.rep.MRR, "—", "—")
			continue
		}
		fmt.Printf("%-20s  %8.4f  %8.4f  %8.4f  %+8.4f  %+8.4f%s\n",
			r.label, r.rep.MeanNDCG, r.rep.MeanRecall, r.rep.MRR,
			r.rep.MeanNDCG-base.MeanNDCG, r.rep.MeanRecall-base.MeanRecall, marker)
	}

	// Verdict: is the best weight materially better than graph-off?
	const eps = 0.005 // ~0.5pt — below this the lane isn't earning its weight knob
	dNDCG := rows[best].rep.MeanNDCG - base.MeanNDCG
	dRecall := rows[best].rep.MeanRecall - base.MeanRecall
	if dNDCG < eps && dRecall < eps {
		fmt.Printf("\nverdict: graph lane INERT — best weight (%s) gains only ΔNDCG=%+.4f ΔRecall=%+.4f over graph-off (ε=%.3f).\n",
			rows[best].label, dNDCG, dRecall, eps)
		fmt.Printf("         GraphLaneWeight is a no-op at this rerank pool; consider removing it (separate decision, see #470).\n")
	} else {
		fmt.Printf("\nverdict: graph lane CONTRIBUTES — best weight (%s) gains ΔNDCG=%+.4f ΔRecall=%+.4f over graph-off (ε=%.3f).\n",
			rows[best].label, dNDCG, dRecall, eps)
		fmt.Printf("         calibrate GraphLaneWeight=%.1f in calibration.yml if this holds across the corpus.\n", rows[best].weight)
	}
	return nil
}

// emitCalibration writes the swept winner back to the calibration artifact at
// path. Only the swept dimensions (fusion mode + alpha) come from the run; the
// other lanes keep the current calibrated values, so the file stays complete.
func emitCalibration(path string, mode store.FusionMode, alpha float32, metric string) error {
	c := store.CalibratedDefaults() // preserve rrfK / graph / rerank lanes
	c.FusionMode = store.FusionModeString(mode)
	if mode == store.FusionLinear {
		c.FusionAlpha = alpha
	}
	c.Provenance.Source = "dex eval --alpha-sweep --emit-calibration"
	c.Provenance.Date = time.Now().UTC().Format("2006-01-02")
	c.Provenance.Metric = metric

	out, err := store.MarshalCalibration(c)
	if err != nil {
		return fmt.Errorf("emit calibration: %w", err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return fmt.Errorf("emit calibration: write %s: %w", path, err)
	}
	fmt.Fprintf(os.Stderr, "dex bench eval: wrote calibration to %s (mode=%s alpha=%.2f) — review & commit the diff\n",
		path, c.FusionMode, c.FusionAlpha)
	return nil
}

func generateEvalGolden(ctx context.Context, root, gPath, mode string, maxCommits, maxFiles, maxPerKind int) error {
	opts := eval.GenOpts{MaxCommits: maxCommits, MaxFiles: maxFiles}
	var gs eval.GoldenSet
	var err error
	switch mode {
	case "blast-radius":
		gs, err = eval.GenerateBlastRadius(ctx, root, opts)
	case "structural":
		gs, err = eval.GenerateStructural(ctx, root, opts)
	case "orphan":
		var ocounts eval.OrphanGenCounts
		gs, ocounts, err = eval.GenerateOrphan(ctx, root, eval.OrphanOpts{MaxFiles: maxFiles, MaxPerKind: maxPerKind})
		if err == nil {
			fmt.Fprintf(os.Stderr, "dex bench eval: orphan gen: imports=%d consts=%d vars=%d\n",
				ocounts.Imports, ocounts.Consts, ocounts.Vars)
		}
	default:
		gs, err = eval.Generate(ctx, root, opts)
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
