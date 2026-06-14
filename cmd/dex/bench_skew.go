package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/alehatsman/dex/internal/eval/corpus"
	"github.com/alehatsman/dex/internal/eval/skew"
	"github.com/alehatsman/dex/internal/proj"
)

const skewUsage = `Usage: dex bench skew run [flags]

Cross-language centrality skew eval (#468 gate-2). Loads the indexed call graph
and reports, per language, the PageRank-mass share against the node-count share
over function/method nodes. The ratio (skew) exposes the resolution-accuracy
distortion documented in docs/architecture.md: Go calls are type-resolved (dense
graph) while tree-sitter languages are name-resolved (typed-receiver calls
dropped, sparser graph), so Go nodes accrue centrality out of proportion to how
many nodes each language contributes. skew > 1 = over-weighted; ~1 = parity.

Like 'dex bench trace', this reads only graph_nodes/graph_edges, so it is
GPU-free: repos are graph-indexed with 'index --graph=only', never embedded.
The number is only meaningful for polyglot repos.

Two modes:
  corpus  (default)   run every manifest repo with 'skew: true'
  --project <path>    run a single project

Flags:
  --manifest path  corpus manifest YAML (default: benchmark/corpus/repos.yml)
  --repos a,b      only run these repos (by name); default: all with skew:true
  --project path   run this project instead of the corpus manifest
  --cache path     checkout cache root (default: <index-base>/corpus)
  --output format  json or md (default: md)
  --check path     compare against a committed baseline JSON; exit 1 if any
                   per-language skew ratio drifts beyond tolerance (>0.05)
`

const skewDriftTol = 0.05

func runBenchSkew(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "run" {
		fmt.Fprint(os.Stderr, skewUsage)
		return flag.ErrHelp
	}
	fs := flag.NewFlagSet("bench skew run", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, skewUsage) }
	manifestPath := fs.String("manifest", filepath.Join("benchmark", "corpus", "repos.yml"), "corpus manifest YAML")
	reposFlag := fs.String("repos", "", "comma-separated repo names to run (default: all with skew:true)")
	projectFlag := fs.String("project", "", "run this project instead of the corpus manifest")
	cacheFlag := fs.String("cache", "", "checkout cache root (default: <index-base>/corpus)")
	outputFmt := fs.String("output", "md", "output format: json or md")
	checkPath := fs.String("check", "", "baseline report JSON to check for skew drift")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	base, err := indexDir()
	if err != nil {
		return fmt.Errorf("dex bench skew: index dir: %w", err)
	}

	var cells []skew.Cell
	if *projectFlag != "" {
		cells, err = skewProject(ctx, base, *projectFlag)
	} else {
		cells, err = skewCorpus(ctx, base, *cacheFlag, *manifestPath, *reposFlag)
	}
	if err != nil {
		return err
	}
	if len(cells) == 0 {
		return fmt.Errorf("dex bench skew: no cells run (check --repos / skew:true / --project)")
	}

	suite := skew.NewSuite(cells)
	switch *outputFmt {
	case "json":
		out, err := suite.JSON()
		if err != nil {
			return fmt.Errorf("dex bench skew: marshal: %w", err)
		}
		fmt.Println(string(out))
	default:
		fmt.Print(suite.Markdown())
	}

	if *checkPath != "" {
		data, err := os.ReadFile(*checkPath)
		if err != nil {
			return fmt.Errorf("dex bench skew: read baseline: %w", err)
		}
		ref, err := skew.LoadSuite(data)
		if err != nil {
			return fmt.Errorf("dex bench skew: %w", err)
		}
		drifts := suite.Drift(ref, skewDriftTol)
		if len(drifts) > 0 {
			for _, d := range drifts {
				fmt.Fprintf(os.Stderr, "  %s\n", d.String())
			}
			return fmt.Errorf("dex bench skew: drift check failed (%d language(s), tol %.2f)", len(drifts), skewDriftTol)
		}
		fmt.Fprintln(os.Stderr, "dex bench skew: drift check passed")
	}
	return nil
}

// skewCorpus runs every manifest repo opted into skew.
func skewCorpus(ctx context.Context, base, cacheFlag, manifestPath, reposFlag string) ([]skew.Cell, error) {
	cacheRoot := cacheFlag
	if cacheRoot == "" {
		cacheRoot = filepath.Join(base, "corpus")
	}
	absManifest, err := filepath.Abs(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("dex bench skew: resolve manifest path: %w", err)
	}
	m, err := corpus.LoadManifest(absManifest)
	if err != nil {
		return nil, fmt.Errorf("dex bench skew: load manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("dex bench skew: invalid manifest: %w", err)
	}
	want := repoFilter(reposFlag)

	var cells []skew.Cell
	for _, spec := range m.Repos {
		if want != nil && !want[spec.Name] {
			continue
		}
		if !spec.Skew {
			continue
		}
		cell, err := skewCorpusRepo(ctx, base, cacheRoot, spec)
		if err != nil {
			return nil, fmt.Errorf("dex bench skew: %s: %w", spec.Name, err)
		}
		cells = append(cells, cell)
	}
	return cells, nil
}

// skewCorpusRepo fetches, graph-indexes (if needed), and computes skew for one
// repo. Mirrors traceCorpusRepo's fetch/index/open flow.
func skewCorpusRepo(ctx context.Context, base, cacheRoot string, spec corpus.RepoSpec) (skew.Cell, error) {
	dir, err := corpus.Ensure(ctx, spec, cacheRoot)
	if err != nil {
		return skew.Cell{}, err
	}
	indexPath := dir
	if spec.IndexSubdir != "" {
		indexPath = filepath.Join(dir, spec.IndexSubdir)
	}

	p, err := proj.Resolve(indexPath, base)
	if err != nil {
		return skew.Cell{}, fmt.Errorf("resolve: %w", err)
	}
	if _, statErr := os.Stat(p.DBPath); statErr != nil {
		if cfgErr := ensureCorpusIndexConfig(indexPath); cfgErr != nil {
			return skew.Cell{}, fmt.Errorf("seed index config: %w", cfgErr)
		}
		fmt.Fprintf(os.Stderr, "dex bench skew: graph-indexing %s (%s)\n", spec.Name, indexPath)
		if ixErr := cmdIndex(ctx, []string{"--graph=only", indexPath}); ixErr != nil {
			return skew.Cell{}, fmt.Errorf("graph-index: %w", ixErr)
		}
	}

	view, err := openTraceView(ctx, p.DBPath)
	if err != nil {
		return skew.Cell{}, err
	}
	fmt.Fprintf(os.Stderr, "dex bench skew: computing skew for %s\n", spec.Name)
	return skew.Cell{Repo: spec.Name, Report: skew.Compute(view)}, nil
}

// skewProject computes skew for a single (already-indexed or to-be-graph-indexed)
// project — the GPU-free, network-free path.
func skewProject(ctx context.Context, base, project string) ([]skew.Cell, error) {
	p, err := proj.Resolve(project, base)
	if err != nil {
		return nil, fmt.Errorf("dex bench skew: resolve project: %w", err)
	}
	if _, statErr := os.Stat(p.DBPath); statErr != nil {
		fmt.Fprintf(os.Stderr, "dex bench skew: graph-indexing %s\n", project)
		if ixErr := cmdIndex(ctx, []string{"--graph=only", project}); ixErr != nil {
			return nil, fmt.Errorf("dex bench skew: graph-index: %w", ixErr)
		}
	}
	view, err := openTraceView(ctx, p.DBPath)
	if err != nil {
		return nil, err
	}
	label := filepath.Base(strings.TrimRight(project, string(os.PathSeparator)))
	return []skew.Cell{{Repo: label, Report: skew.Compute(view)}}, nil
}
