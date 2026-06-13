package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/alehatsman/dex/internal/corpus"
	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/store"
	"golang.org/x/sync/errgroup"
)

const corpusUsage = `Usage: dex bench corpus run [flags]

Multi-repo retrieval eval. Fetches each repo in the corpus manifest at its
pinned commit, indexes it (if not already indexed), and scores the live Search
path against every declared golden set — curated query sets and/or auto-labels
mined from the repo's own history — reporting NDCG/Recall/MRR per (repo, set).

Unlike 'dex bench eval' (single repo, this project), the corpus validates
retrieval across multiple languages so tuning isn't overfit to one codebase.
The regression gate fails when ANY single (repo, set) cell drops past tolerance,
not just the aggregate mean.

Flags:
  --manifest path  corpus manifest YAML (default: benchmark/corpus/repos.yml)
  --repos a,b      only run these repos (by name); default: all
  --smoke          first curated set per repo only, skip generated sets (fast)
  --k int          retrieval depth (default: 10)
  --cache path     checkout cache root (default: <index-base>/corpus)
  --output format  json or md (default: md)
  --check path     compare against a committed baseline JSON; exit 1 on any
                   per-cell regression (>0.02)
  --jobs int       repos to score concurrently (0 = auto, capped at 4) — overlaps
                   one repo's git-mining/indexing with another's GPU scoring

Query-set paths in the manifest are resolved relative to the manifest's
directory. Requires a live embed endpoint (DEX_EMBED_URL / ollama) — like
'dex bench eval', this is a local gate, not a container-CI step.
`

// maxCorpusJobs caps the repo-level scoring fan-out. The repos are independent
// (own store/embed client), but they share the embed/rerank endpoints; past ~4
// concurrent repos the GPU servers are saturated and more just queues, while the
// per-query DEX_EVAL_CONCURRENCY fan-out already keeps each repo busy.
const maxCorpusJobs = 4

func runCorpus(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "run" {
		fmt.Fprint(os.Stderr, corpusUsage)
		return flag.ErrHelp
	}
	fs := flag.NewFlagSet("bench corpus run", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, corpusUsage) }
	manifestPath := fs.String("manifest", filepath.Join("benchmark", "corpus", "repos.yml"), "corpus manifest YAML")
	reposFlag := fs.String("repos", "", "comma-separated repo names to run (default: all)")
	smoke := fs.Bool("smoke", false, "first curated set per repo only, skip generated sets")
	k := fs.Int("k", 10, "retrieval depth")
	cacheFlag := fs.String("cache", "", "checkout cache root (default: <index-base>/corpus)")
	outputFmt := fs.String("output", "md", "output format: json or md")
	checkPath := fs.String("check", "", "baseline report JSON to check for per-cell regression")
	jobs := fs.Int("jobs", 0, "repos to score concurrently (0 = auto, capped at maxCorpusJobs)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	base, err := indexDir()
	if err != nil {
		return fmt.Errorf("dex bench corpus: index dir: %w", err)
	}
	cacheRoot := *cacheFlag
	if cacheRoot == "" {
		cacheRoot = filepath.Join(base, "corpus")
	}

	absManifest, err := filepath.Abs(*manifestPath)
	if err != nil {
		return fmt.Errorf("dex bench corpus: resolve manifest path: %w", err)
	}
	m, err := corpus.LoadManifest(absManifest)
	if err != nil {
		return fmt.Errorf("dex bench corpus: load manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return fmt.Errorf("dex bench corpus: invalid manifest: %w", err)
	}
	manifestDir := filepath.Dir(absManifest)
	want := repoFilter(*reposFlag)

	// Filter repos, preserving manifest order so cells stay deterministic.
	var specs []corpus.RepoSpec
	for _, spec := range m.Repos {
		if want != nil && !want[spec.Name] {
			continue
		}
		specs = append(specs, spec)
	}

	jobsN := *jobs
	if jobsN <= 0 {
		jobsN = len(specs) // auto: all repos at once, capped below
	}
	if jobsN > maxCorpusJobs {
		jobsN = maxCorpusJobs
	}
	if jobsN < 1 {
		jobsN = 1
	}

	// Score repos concurrently. Each runCorpusRepo opens its own store + embed
	// client on a distinct checkout/index, so they're independent; the only
	// shared resources are the embed/rerank endpoints, which queue server-side.
	// A bounded pool overlaps one repo's git-mining/indexing (CPU/IO, GPU idle)
	// with another's scoring (GPU) — closing the corpus runner's remaining
	// GPU-idle gap (#331). results[i] is written by a unique index so cell order
	// is deterministic regardless of completion order.
	results := make([][]corpus.LabeledReport, len(specs))
	eg, egctx := errgroup.WithContext(ctx)
	eg.SetLimit(jobsN)
	for i, spec := range specs {
		i, spec := i, spec
		eg.Go(func() error {
			cells, err := runCorpusRepo(egctx, base, cacheRoot, manifestDir, spec, *k, *smoke)
			if err != nil {
				return fmt.Errorf("%s: %w", spec.Name, err)
			}
			results[i] = cells
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return fmt.Errorf("dex bench corpus: %w", err)
	}

	var allCells []corpus.LabeledReport
	for _, cells := range results {
		allCells = append(allCells, cells...)
	}
	if len(allCells) == 0 {
		return fmt.Errorf("dex bench corpus: no cells scored (check --repos / manifest)")
	}

	rep := corpus.Compute(allCells, *k)
	switch *outputFmt {
	case "json":
		out, err := rep.JSON()
		if err != nil {
			return fmt.Errorf("dex bench corpus: marshal: %w", err)
		}
		fmt.Println(string(out))
	default:
		fmt.Print(rep.Markdown())
	}

	if *checkPath != "" {
		data, err := os.ReadFile(*checkPath)
		if err != nil {
			return fmt.Errorf("dex bench corpus: read baseline: %w", err)
		}
		ref, err := corpus.LoadReport(data)
		if err != nil {
			return fmt.Errorf("dex bench corpus: %w", err)
		}
		const tol = 0.02
		regs := rep.Regressions(ref, tol)
		if len(regs) > 0 {
			for _, r := range regs {
				fmt.Fprintf(os.Stderr, "  %s\n", r.String())
			}
			return fmt.Errorf("dex bench corpus: regression check failed (%d cell(s), tol %.2f)", len(regs), tol)
		}
		fmt.Fprintln(os.Stderr, "dex bench corpus: regression check passed")
	}
	return nil
}

// runCorpusRepo fetches, indexes (if needed), and scores one repo. Indexing and
// store/embed wiring mirror runEval (cmd/dex/eval.go); the eval orchestration
// lives in internal/corpus.RunRepo.
func runCorpusRepo(ctx context.Context, base, cacheRoot, manifestDir string, spec corpus.RepoSpec, k int, smoke bool) ([]corpus.LabeledReport, error) {
	dir, err := corpus.Ensure(ctx, spec, cacheRoot)
	if err != nil {
		return nil, err
	}
	indexPath := dir
	if spec.IndexSubdir != "" {
		indexPath = filepath.Join(dir, spec.IndexSubdir)
	}

	p, err := proj.Resolve(indexPath, base)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", spec.Name, err)
	}
	if _, statErr := os.Stat(p.DBPath); statErr != nil {
		// Indexing is opt-in via .dex/config.yml index.include. Corpus repos
		// are third-party checkouts with no dex config, so seed a minimal one
		// (include everything from the indexed root) before indexing. Endpoints
		// and models come from the ambient env, which takes precedence.
		if cfgErr := ensureCorpusIndexConfig(indexPath); cfgErr != nil {
			return nil, fmt.Errorf("seed index config for %s: %w", spec.Name, cfgErr)
		}
		fmt.Fprintf(os.Stderr, "dex bench corpus: indexing %s (%s)\n", spec.Name, indexPath)
		if ixErr := cmdIndex(ctx, []string{indexPath}); ixErr != nil {
			return nil, fmt.Errorf("index %s: %w", spec.Name, ixErr)
		}
	}

	st, err := store.OpenWith(ctx, p.DBPath, storeOpts())
	if err != nil {
		return nil, fmt.Errorf("open store %s: %w", spec.Name, err)
	}
	defer func() { _ = st.Close() }()

	stats, _ := st.Stats(ctx)
	em := newEmbedClient(stats.EmbedModel)

	runSpec := resolveQuerySets(spec, manifestDir)
	if smoke {
		runSpec = applySmoke(runSpec)
	}
	fmt.Fprintf(os.Stderr, "dex bench corpus: scoring %s (%d query set(s)%s)\n",
		spec.Name, len(runSpec.QuerySets), genLabel(runSpec))
	// Cache git-mined golden sets under the corpus cache root so a sweep that
	// re-scores these repos under many fusion settings mines git only once.
	genCacheDir := filepath.Join(cacheRoot, ".gensets")
	return corpus.RunRepo(ctx, em, st, runSpec, indexPath, k, genCacheDir)
}

// ensureCorpusIndexConfig writes a minimal .dex/config.yml under root if none
// exists, opting the whole checkout into indexing. Endpoints/models are left to
// the ambient env (DEX_EMBED_URL etc.), which takes precedence over the file.
func ensureCorpusIndexConfig(root string) error {
	cfgDir := filepath.Join(root, ".dex")
	cfgPath := filepath.Join(cfgDir, "config.yml")
	if _, err := os.Stat(cfgPath); err == nil {
		return nil // respect an existing config
	}
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		return err
	}
	// index.include uses gitignore grammar — an extension glob matches that
	// extension at any depth. These mirror eval's codeExts so the indexed set
	// aligns with the golden-set's relevant (code) files. The repo's own
	// .gitignore still excludes vendored/build dirs.
	const body = "# Generated by `dex bench corpus` to opt this checkout into indexing.\n" +
		"# Endpoints/models come from the environment (DEX_EMBED_URL etc.).\n" +
		"index:\n  include:\n" +
		"    - \"*.go\"\n    - \"*.py\"\n    - \"*.ts\"\n    - \"*.tsx\"\n    - \"*.js\"\n" +
		"    - \"*.jsx\"\n    - \"*.rs\"\n    - \"*.java\"\n    - \"*.c\"\n    - \"*.h\"\n" +
		"    - \"*.cc\"\n    - \"*.cpp\"\n    - \"*.hpp\"\n    - \"*.rb\"\n    - \"*.sh\"\n"
	return os.WriteFile(cfgPath, []byte(body), 0o644)
}

// resolveQuerySets rewrites the spec's query-set paths to absolute, relative to
// the manifest's directory.
func resolveQuerySets(spec corpus.RepoSpec, manifestDir string) corpus.RepoSpec {
	out := make([]string, len(spec.QuerySets))
	for i, qs := range spec.QuerySets {
		if filepath.IsAbs(qs) {
			out[i] = qs
		} else {
			out[i] = filepath.Join(manifestDir, qs)
		}
	}
	spec.QuerySets = out
	return spec
}

// applySmoke trims a spec to its first curated set and disables generated sets,
// for fast local iteration.
func applySmoke(spec corpus.RepoSpec) corpus.RepoSpec {
	if len(spec.QuerySets) > 1 {
		spec.QuerySets = spec.QuerySets[:1]
	}
	spec.Gen = corpus.GenConfig{}
	return spec
}

func genLabel(spec corpus.RepoSpec) string {
	var on []string
	if spec.Gen.GitHistory.Enabled {
		on = append(on, "git-history")
	}
	if spec.Gen.BlastRadius.Enabled {
		on = append(on, "blast-radius")
	}
	if spec.Gen.Structural.Enabled {
		on = append(on, "structural")
	}
	if len(on) == 0 {
		return ""
	}
	return " + gen:" + strings.Join(on, ",")
}

func repoFilter(csv string) map[string]bool {
	csv = strings.TrimSpace(csv)
	if csv == "" {
		return nil
	}
	out := map[string]bool{}
	for _, name := range strings.Split(csv, ",") {
		if n := strings.TrimSpace(name); n != "" {
			out[n] = true
		}
	}
	return out
}
