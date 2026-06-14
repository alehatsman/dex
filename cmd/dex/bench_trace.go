package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/alehatsman/dex/internal/eval/corpus"
	"github.com/alehatsman/dex/internal/eval/trace"
	"github.com/alehatsman/dex/internal/graphquery"
	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/store"
)

const traceUsage = `Usage: dex bench trace run [flags]

Cross-language trace-precision eval (#468/#496). Loads the indexed call graph
and scores its callers/callees against hand-verified gold probes, reporting
per-(repo, lang) precision/recall/F1. This is the measurement instrument that
nav-bench cannot provide — nav-bench's golden is Go-only, so it can't detect a
cross-language trace regression when the per-language extractors change.

Unlike 'dex bench corpus' (live Search, needs an embed endpoint), trace scoring
reads only graph_nodes/graph_edges, so it is GPU-free: repos are graph-indexed
with 'index --graph=only' and never embedded.

Two modes:
  corpus  (default)   score every manifest repo that declares trace_sets
  --project <path>    score a single project against --gold file(s) instead

Flags:
  --manifest path  corpus manifest YAML (default: benchmark/corpus/repos.yml)
  --repos a,b      only run these repos (by name); default: all with trace_sets
  --project path   score this project instead of the corpus manifest
  --gold a,b       gold JSON file(s) for --project mode (comma-separated)
  --cache path     checkout cache root (default: <index-base>/corpus)
  --output format  json or md (default: md)
  --check path     compare against a committed baseline JSON; exit 1 on any
                   per-cell regression (>0.02)

Trace-set paths in the manifest are resolved relative to the manifest's
directory.
`

const traceRegressTol = 0.02

func runBenchTrace(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "run" {
		fmt.Fprint(os.Stderr, traceUsage)
		return flag.ErrHelp
	}
	fs := flag.NewFlagSet("bench trace run", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, traceUsage) }
	manifestPath := fs.String("manifest", filepath.Join("benchmark", "corpus", "repos.yml"), "corpus manifest YAML")
	reposFlag := fs.String("repos", "", "comma-separated repo names to run (default: all with trace_sets)")
	projectFlag := fs.String("project", "", "score this project instead of the corpus manifest")
	goldFlag := fs.String("gold", "", "gold JSON file(s) for --project mode (comma-separated)")
	cacheFlag := fs.String("cache", "", "checkout cache root (default: <index-base>/corpus)")
	outputFmt := fs.String("output", "md", "output format: json or md")
	checkPath := fs.String("check", "", "baseline report JSON to check for per-cell regression")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	base, err := indexDir()
	if err != nil {
		return fmt.Errorf("dex bench trace: index dir: %w", err)
	}

	var cells []trace.Report
	if *projectFlag != "" {
		cells, err = traceProject(ctx, base, *projectFlag, *goldFlag)
	} else {
		cells, err = traceCorpus(ctx, base, *cacheFlag, *manifestPath, *reposFlag)
	}
	if err != nil {
		return err
	}
	if len(cells) == 0 {
		return fmt.Errorf("dex bench trace: no cells scored (check --repos / trace_sets / --gold)")
	}

	suite := trace.Compute(cells)
	switch *outputFmt {
	case "json":
		out, err := suite.JSON()
		if err != nil {
			return fmt.Errorf("dex bench trace: marshal: %w", err)
		}
		fmt.Println(string(out))
	default:
		fmt.Print(suite.Markdown())
	}

	if *checkPath != "" {
		data, err := os.ReadFile(*checkPath)
		if err != nil {
			return fmt.Errorf("dex bench trace: read baseline: %w", err)
		}
		ref, err := trace.LoadSuite(data)
		if err != nil {
			return fmt.Errorf("dex bench trace: %w", err)
		}
		regs := suite.Regressions(ref, traceRegressTol)
		if len(regs) > 0 {
			for _, r := range regs {
				fmt.Fprintf(os.Stderr, "  %s\n", r.String())
			}
			return fmt.Errorf("dex bench trace: regression check failed (%d cell(s), tol %.2f)", len(regs), traceRegressTol)
		}
		fmt.Fprintln(os.Stderr, "dex bench trace: regression check passed")
	}
	return nil
}

// traceCorpus scores every manifest repo that declares trace_sets.
func traceCorpus(ctx context.Context, base, cacheFlag, manifestPath, reposFlag string) ([]trace.Report, error) {
	cacheRoot := cacheFlag
	if cacheRoot == "" {
		cacheRoot = filepath.Join(base, "corpus")
	}
	absManifest, err := filepath.Abs(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("dex bench trace: resolve manifest path: %w", err)
	}
	m, err := corpus.LoadManifest(absManifest)
	if err != nil {
		return nil, fmt.Errorf("dex bench trace: load manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("dex bench trace: invalid manifest: %w", err)
	}
	manifestDir := filepath.Dir(absManifest)
	want := repoFilter(reposFlag)

	var cells []trace.Report
	for _, spec := range m.Repos {
		if want != nil && !want[spec.Name] {
			continue
		}
		if len(spec.TraceSets) == 0 {
			continue
		}
		repoCells, err := traceCorpusRepo(ctx, base, cacheRoot, manifestDir, spec)
		if err != nil {
			return nil, fmt.Errorf("dex bench trace: %s: %w", spec.Name, err)
		}
		cells = append(cells, repoCells...)
	}
	return cells, nil
}

// traceCorpusRepo fetches, graph-indexes (if needed), and scores one repo's
// trace sets. Mirrors runCorpusRepo's fetch/index/open flow but indexes
// --graph=only (no embeddings) and scores the graph instead of Search.
func traceCorpusRepo(ctx context.Context, base, cacheRoot, manifestDir string, spec corpus.RepoSpec) ([]trace.Report, error) {
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
		return nil, fmt.Errorf("resolve: %w", err)
	}
	if _, statErr := os.Stat(p.DBPath); statErr != nil {
		if cfgErr := ensureCorpusIndexConfig(indexPath); cfgErr != nil {
			return nil, fmt.Errorf("seed index config: %w", cfgErr)
		}
		fmt.Fprintf(os.Stderr, "dex bench trace: graph-indexing %s (%s)\n", spec.Name, indexPath)
		if ixErr := cmdIndex(ctx, []string{"--graph=only", indexPath}); ixErr != nil {
			return nil, fmt.Errorf("graph-index: %w", ixErr)
		}
	}

	view, err := openTraceView(ctx, p.DBPath)
	if err != nil {
		return nil, err
	}

	goldPaths := resolveTraceSets(spec, manifestDir)
	fmt.Fprintf(os.Stderr, "dex bench trace: scoring %s (%d gold set(s))\n", spec.Name, len(goldPaths))
	return scoreGoldFiles(view, spec.Name, goldPaths)
}

// traceProject scores a single (already-indexed or to-be-graph-indexed) project
// against explicit --gold files. This is the GPU-free, network-free path used to
// score dex itself in CI.
func traceProject(ctx context.Context, base, project, goldCSV string) ([]trace.Report, error) {
	goldPaths := splitCSV(goldCSV)
	if len(goldPaths) == 0 {
		return nil, fmt.Errorf("dex bench trace: --project requires --gold")
	}
	p, err := proj.Resolve(project, base)
	if err != nil {
		return nil, fmt.Errorf("dex bench trace: resolve project: %w", err)
	}
	if _, statErr := os.Stat(p.DBPath); statErr != nil {
		fmt.Fprintf(os.Stderr, "dex bench trace: graph-indexing %s\n", project)
		if ixErr := cmdIndex(ctx, []string{"--graph=only", project}); ixErr != nil {
			return nil, fmt.Errorf("dex bench trace: graph-index: %w", ixErr)
		}
	}
	view, err := openTraceView(ctx, p.DBPath)
	if err != nil {
		return nil, err
	}
	return scoreGoldFiles(view, "", goldPaths)
}

// openTraceView opens the store and loads the graph view, treating an empty
// graph as a hard error — scoring against no edges would silently report every
// probe unresolved, masking a missing index.
func openTraceView(ctx context.Context, dbPath string) (*graphquery.View, error) {
	st, err := store.OpenWith(ctx, dbPath, storeOpts())
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = st.Close() }()

	view, err := graphquery.Load(ctx, st)
	if err != nil {
		return nil, fmt.Errorf("load graph: %w", err)
	}
	if view == nil {
		return nil, fmt.Errorf("index has no call graph — reindex with `dex index --graph=only`")
	}
	return view, nil
}

// scoreGoldFiles scores each gold file against view. repoFallback names the cell
// repo when a gold file omits its own repo field.
func scoreGoldFiles(view *graphquery.View, repoFallback string, goldPaths []string) ([]trace.Report, error) {
	var cells []trace.Report
	for _, gp := range goldPaths {
		gold, err := trace.LoadGold(gp)
		if err != nil {
			return nil, fmt.Errorf("load gold %s: %w", gp, err)
		}
		rep := trace.Score(view, gold)
		if rep.Repo == "" {
			rep.Repo = repoFallback
		}
		rep.Set = setLabel(gp)
		cells = append(cells, rep)
	}
	return cells, nil
}

// resolveTraceSets rewrites the spec's trace-set paths to absolute, relative to
// the manifest's directory.
func resolveTraceSets(spec corpus.RepoSpec, manifestDir string) []string {
	out := make([]string, len(spec.TraceSets))
	for i, ts := range spec.TraceSets {
		if filepath.IsAbs(ts) {
			out[i] = ts
		} else {
			out[i] = filepath.Join(manifestDir, ts)
		}
	}
	return out
}

func setLabel(path string) string {
	return strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
}

func splitCSV(csv string) []string {
	var out []string
	for _, s := range strings.Split(csv, ",") {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	return out
}
