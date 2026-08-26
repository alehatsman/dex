package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/alehatsman/dex/internal/eval"
	"github.com/alehatsman/dex/internal/eval/cochange"
	"github.com/alehatsman/dex/internal/eval/corpus"
	"github.com/alehatsman/dex/internal/graphquery"
	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/store"
)

const cochangeUsage = `Usage: dex bench cochange run [flags]

Co-change structural-coverage eval (#555). For each repo's blast-radius golden
set — anchor file → the OTHER files co-changed with it in a commit — this
measures how much of the SRC-ONLY subset (test-tainted gold excluded) is
reachable from the anchor through the structural (calls/imports) graph, at 1
and 2 hops.

It answers the #555 question "can the graph lane re-rank the blast-radius gap?"
Where two-hop reachability is low, the co-change coupling is non-structural —
history reveals it, the call/import graph does not — so no graph/fusion reweight
can close the gap. The pinned number IS the ceiling that justifies parking #555.

Like 'dex bench skew'/'trace', this reads only graph_nodes/graph_edges plus the
git-mined blast-radius gold, so it is GPU-free: repos are graph-indexed with
'index --graph=only', never embedded. The number is meaningful per repo.

Two modes:
  corpus  (default)   run every manifest repo with gen.blast_radius enabled
  --project <path>    run a single project (mines its own git history)

Flags:
  --manifest path  corpus manifest YAML (default: benchmark/corpus/repos.yml)
  --repos a,b      only run these repos (by name); default: all blast_radius repos
  --project path   run this project instead of the corpus manifest
  --lang name      language for the test-file heuristic in --project mode
  --cache path     checkout cache root (default: <index-base>/corpus)
  --output format  json or md (default: md)
  --check path     compare against a committed baseline JSON; exit 1 if any
                   per-repo two-hop reachability drifts beyond tolerance (>0.05)
`

const cochangeDriftTol = 0.05

func runBenchCochange(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "run" {
		fmt.Fprint(os.Stderr, cochangeUsage)
		return flag.ErrHelp
	}
	fs := flag.NewFlagSet("bench cochange run", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, cochangeUsage) }
	manifestPath := fs.String("manifest", filepath.Join("benchmark", "corpus", "repos.yml"), "corpus manifest YAML")
	reposFlag := fs.String("repos", "", "comma-separated repo names to run (default: all blast_radius repos)")
	projectFlag := fs.String("project", "", "run this project instead of the corpus manifest")
	langFlag := fs.String("lang", "", "language for the test-file heuristic in --project mode")
	cacheFlag := fs.String("cache", "", "checkout cache root (default: <index-base>/corpus)")
	outputFmt := fs.String("output", "md", "output format: json or md")
	checkPath := fs.String("check", "", "baseline report JSON to check for reachability drift")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	base, err := indexDir()
	if err != nil {
		return fmt.Errorf("dex bench cochange: index dir: %w", err)
	}

	var cells []cochange.Cell
	if *projectFlag != "" {
		cells, err = cochangeProject(ctx, base, *projectFlag, *langFlag)
	} else {
		cells, err = cochangeCorpus(ctx, base, *cacheFlag, *manifestPath, *reposFlag)
	}
	if err != nil {
		return err
	}
	if len(cells) == 0 {
		return fmt.Errorf("dex bench cochange: no cells run (check --repos / gen.blast_radius / --project)")
	}

	suite := cochange.NewSuite(cells)
	switch *outputFmt {
	case "json":
		out, err := suite.JSON()
		if err != nil {
			return fmt.Errorf("dex bench cochange: marshal: %w", err)
		}
		fmt.Println(string(out))
	default:
		fmt.Print(suite.Markdown())
	}

	if *checkPath != "" {
		data, err := os.ReadFile(*checkPath)
		if err != nil {
			return fmt.Errorf("dex bench cochange: read baseline: %w", err)
		}
		ref, err := cochange.LoadSuite(data)
		if err != nil {
			return fmt.Errorf("dex bench cochange: %w", err)
		}
		drifts := suite.Drift(ref, cochangeDriftTol)
		if len(drifts) > 0 {
			for _, d := range drifts {
				fmt.Fprintf(os.Stderr, "  %s\n", d.String())
			}
			return fmt.Errorf("dex bench cochange: drift check failed (%d repo(s), tol %.2f)", len(drifts), cochangeDriftTol)
		}
		fmt.Fprintln(os.Stderr, "dex bench cochange: drift check passed")
	}
	return nil
}

// cochangeCorpus runs every manifest repo with blast-radius generation enabled.
func cochangeCorpus(ctx context.Context, base, cacheFlag, manifestPath, reposFlag string) ([]cochange.Cell, error) {
	cacheRoot := cacheFlag
	if cacheRoot == "" {
		cacheRoot = filepath.Join(base, "corpus")
	}
	absManifest, err := filepath.Abs(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("dex bench cochange: resolve manifest path: %w", err)
	}
	m, err := corpus.LoadManifest(absManifest)
	if err != nil {
		return nil, fmt.Errorf("dex bench cochange: load manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("dex bench cochange: invalid manifest: %w", err)
	}
	want := repoFilter(reposFlag)

	var cells []cochange.Cell
	for _, spec := range m.Repos {
		if want != nil && !want[spec.Name] {
			continue
		}
		if !spec.Gen.BlastRadius.Enabled {
			continue
		}
		cell, ok, err := cochangeCorpusRepo(ctx, base, cacheRoot, spec)
		if err != nil {
			return nil, fmt.Errorf("dex bench cochange: %s: %w", spec.Name, err)
		}
		if !ok {
			fmt.Fprintf(os.Stderr, "dex bench cochange: skip %s (%s): no call graph — language has no graph extractor\n", spec.Name, primaryLang(spec))
			continue
		}
		cells = append(cells, cell)
	}
	return cells, nil
}

// cochangeCorpusRepo fetches, graph-indexes (if needed), mines the blast-radius
// gold, and computes coverage for one repo. Mirrors skewCorpusRepo's flow; the
// gold mining uses the same indexPath the graph is indexed from so anchor/gold
// paths share the graph view's path namespace (matters for IndexSubdir repos).
func cochangeCorpusRepo(ctx context.Context, base, cacheRoot string, spec corpus.RepoSpec) (cochange.Cell, bool, error) {
	dir, err := corpus.Ensure(ctx, spec, cacheRoot)
	if err != nil {
		return cochange.Cell{}, false, err
	}
	indexPath := dir
	if spec.IndexSubdir != "" {
		indexPath = filepath.Join(dir, spec.IndexSubdir)
	}

	p, err := proj.Resolve(indexPath, base)
	if err != nil {
		return cochange.Cell{}, false, fmt.Errorf("resolve: %w", err)
	}
	if _, statErr := os.Stat(p.DBPath); statErr != nil {
		if cfgErr := ensureCorpusIndexConfig(indexPath); cfgErr != nil {
			return cochange.Cell{}, false, fmt.Errorf("seed index config: %w", cfgErr)
		}
		fmt.Fprintf(os.Stderr, "dex bench cochange: graph-indexing %s (%s)\n", spec.Name, indexPath)
		if ixErr := cmdIndex(ctx, []string{"--graph=only", indexPath}); ixErr != nil {
			return cochange.Cell{}, false, fmt.Errorf("graph-index: %w", ixErr)
		}
	}

	// Languages with no graph extractor (C/C++) index without a call graph;
	// structural coverage is not measurable, so skip rather than report a
	// misleading 0% reachability.
	view, err := loadGraphView(ctx, p.DBPath)
	if err != nil {
		return cochange.Cell{}, false, err
	}
	if view == nil {
		return cochange.Cell{}, false, nil
	}

	opts := eval.GenOpts{MaxCommits: spec.Gen.BlastRadius.MaxCommits, MaxFiles: spec.Gen.BlastRadius.MaxFiles}
	gs, err := eval.GenerateBlastRadius(ctx, indexPath, opts)
	if err != nil {
		return cochange.Cell{}, false, fmt.Errorf("mine blast-radius gold: %w", err)
	}
	fmt.Fprintf(os.Stderr, "dex bench cochange: scoring %s (%d blast-radius queries)\n", spec.Name, len(gs.Queries))
	lang := primaryLang(spec)
	return cochange.Cell{
		Repo:         spec.Name,
		Report:       cochange.Compute(view, gs.Queries, lang),
		WithCoChange: cochange.ComputeWithCoChange(view, gs.Queries, lang),
	}, true, nil
}

// loadGraphView opens the index and loads its call graph. Unlike openTraceView
// it returns (nil, nil) when the index has no call graph (e.g. C/C++, which
// have no extractor) so the caller can skip rather than abort.
func loadGraphView(ctx context.Context, dbPath string) (*graphquery.View, error) {
	st, err := store.OpenWith(ctx, dbPath, storeOpts())
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = st.Close() }()
	view, err := graphquery.Load(ctx, st)
	if err != nil {
		return nil, fmt.Errorf("load graph: %w", err)
	}
	return view, nil
}

// cochangeProject computes coverage for a single project, mining its own git
// history. --lang sets the test-file heuristic (default: generic).
func cochangeProject(ctx context.Context, base, project, lang string) ([]cochange.Cell, error) {
	p, err := proj.Resolve(project, base)
	if err != nil {
		return nil, fmt.Errorf("dex bench cochange: resolve project: %w", err)
	}
	if _, statErr := os.Stat(p.DBPath); statErr != nil {
		fmt.Fprintf(os.Stderr, "dex bench cochange: graph-indexing %s\n", project)
		if ixErr := cmdIndex(ctx, []string{"--graph=only", project}); ixErr != nil {
			return nil, fmt.Errorf("dex bench cochange: graph-index: %w", ixErr)
		}
	}
	view, err := openTraceView(ctx, p.DBPath)
	if err != nil {
		return nil, err
	}
	gs, err := eval.GenerateBlastRadius(ctx, project, eval.GenOpts{})
	if err != nil {
		return nil, fmt.Errorf("dex bench cochange: mine blast-radius gold: %w", err)
	}
	label := filepath.Base(strings.TrimRight(project, string(os.PathSeparator)))
	return []cochange.Cell{{
		Repo:         label,
		Report:       cochange.Compute(view, gs.Queries, lang),
		WithCoChange: cochange.ComputeWithCoChange(view, gs.Queries, lang),
	}}, nil
}

// primaryLang is the first declared language of a corpus repo, used for the
// test-file heuristic. Empty when unset (falls back to the generic rule).
func primaryLang(spec corpus.RepoSpec) string {
	if len(spec.Languages) == 0 {
		return ""
	}
	return spec.Languages[0]
}
