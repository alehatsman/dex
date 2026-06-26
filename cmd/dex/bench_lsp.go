package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alehatsman/dex/internal/eval/corpus"
	"github.com/alehatsman/dex/internal/eval/lsprecall"
	"github.com/alehatsman/dex/internal/eval/trace"
	"github.com/alehatsman/dex/internal/proj"
)

const lspUsage = `Usage: dex bench lsp run [flags]

Phase-2 recall gate for #604 Tier 2 (LSP for non-Go). Measures how many
callers/callees an LSP server finds versus what the tree-sitter graph already
covers, for each probe in the corpus trace golden sets.

Decision rule (applied per repo+language cell):
  mean_recall ≥ 0.70  → graph covers ~70%+ of what LSP finds; park Tier 2
  mean_recall < 0.50  → LSP finds 2×+ more; ship Tier 2

Only repos tagged lsp_recall:true in the manifest are scored. Currently:
zod (TypeScript) — requires typescript-language-server on PATH.

Like 'dex bench trace', this reads only graph_nodes/graph_edges: repos are
graph-indexed with 'index --graph=only' and never embedded (GPU-free).

Flags:
  --manifest path     corpus manifest YAML (default: benchmark/corpus/repos.yml)
  --repos a,b         only run these repos (default: all with lsp_recall:true)
  --project path      run this project instead of the corpus manifest
  --gold path         gold JSON file for --project mode
  --cache path        checkout cache root (default: <index-base>/corpus)
  --ts-server cmd     TypeScript LSP server command (default: typescript-language-server --stdio)
  --timeout duration  per-probe LSP timeout (default: 30s)
  --output format     json or md (default: md)
  --check path        compare against a committed baseline JSON; exit 1 on drift > 0.05
`

const lspDriftTol = 0.05

func runBenchLSP(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "run" {
		fmt.Fprint(os.Stderr, lspUsage)
		return flag.ErrHelp
	}
	fs := flag.NewFlagSet("bench lsp run", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, lspUsage) }
	manifestPath := fs.String("manifest", filepath.Join("benchmark", "corpus", "repos.yml"), "corpus manifest YAML")
	reposFlag := fs.String("repos", "", "comma-separated repo names to run")
	projectFlag := fs.String("project", "", "run this project instead of the corpus manifest")
	goldFlag := fs.String("gold", "", "gold JSON file for --project mode")
	cacheFlag := fs.String("cache", "", "checkout cache root (default: <index-base>/corpus)")
	tsServerFlag := fs.String("ts-server", "typescript-language-server --stdio", "TypeScript LSP server command")
	timeoutFlag := fs.Duration("timeout", 30*time.Second, "per-probe LSP timeout")
	outputFmt := fs.String("output", "md", "output format: json or md")
	checkPath := fs.String("check", "", "baseline report JSON to check for recall drift")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	base, err := indexDir()
	if err != nil {
		return fmt.Errorf("dex bench lsp: index dir: %w", err)
	}

	tsCmd := strings.Fields(*tsServerFlag)

	var cells []lsprecall.Cell
	if *projectFlag != "" {
		cells, err = lspProject(ctx, base, *projectFlag, *goldFlag, tsCmd, *timeoutFlag)
	} else {
		cells, err = lspCorpus(ctx, base, *cacheFlag, *manifestPath, *reposFlag, tsCmd, *timeoutFlag)
	}
	if err != nil {
		return err
	}
	if len(cells) == 0 {
		return fmt.Errorf("dex bench lsp: no cells run (check --repos / lsp_recall:true / --project)")
	}

	suite := lsprecall.NewSuite(cells)
	switch *outputFmt {
	case "json":
		out, err := suite.JSON()
		if err != nil {
			return fmt.Errorf("dex bench lsp: marshal: %w", err)
		}
		fmt.Println(string(out))
	default:
		fmt.Print(suite.Markdown())
	}

	if *checkPath != "" {
		data, err := os.ReadFile(*checkPath)
		if err != nil {
			return fmt.Errorf("dex bench lsp: read baseline: %w", err)
		}
		ref, err := lsprecall.LoadSuite(data)
		if err != nil {
			return fmt.Errorf("dex bench lsp: %w", err)
		}
		drifts := suite.Drift(ref, lspDriftTol)
		if len(drifts) > 0 {
			for _, d := range drifts {
				fmt.Fprintf(os.Stderr, "  %s\n", d.String())
			}
			return fmt.Errorf("dex bench lsp: drift check failed (%d cell(s), tol %.2f)", len(drifts), lspDriftTol)
		}
		fmt.Fprintln(os.Stderr, "dex bench lsp: drift check passed")
	}
	return nil
}

func lspCorpus(ctx context.Context, base, cacheFlag, manifestPath, reposFlag string, tsCmd []string, timeout time.Duration) ([]lsprecall.Cell, error) {
	cacheRoot := cacheFlag
	if cacheRoot == "" {
		cacheRoot = filepath.Join(base, "corpus")
	}
	absManifest, err := filepath.Abs(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("dex bench lsp: resolve manifest path: %w", err)
	}
	m, err := corpus.LoadManifest(absManifest)
	if err != nil {
		return nil, fmt.Errorf("dex bench lsp: load manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("dex bench lsp: invalid manifest: %w", err)
	}
	want := repoFilter(reposFlag)

	var cells []lsprecall.Cell
	for _, spec := range m.Repos {
		if want != nil && !want[spec.Name] {
			continue
		}
		if !spec.LSPRecall {
			continue
		}
		cell, err := lspCorpusRepo(ctx, base, cacheRoot, spec, tsCmd, timeout)
		if err != nil {
			return nil, fmt.Errorf("dex bench lsp: %s: %w", spec.Name, err)
		}
		cells = append(cells, cell)
	}
	return cells, nil
}

func lspCorpusRepo(ctx context.Context, base, cacheRoot string, spec corpus.RepoSpec, tsCmd []string, timeout time.Duration) (lsprecall.Cell, error) {
	dir, err := corpus.Ensure(ctx, spec, cacheRoot)
	if err != nil {
		return lsprecall.Cell{}, err
	}
	indexPath := dir
	if spec.IndexSubdir != "" {
		indexPath = filepath.Join(dir, spec.IndexSubdir)
	}

	p, err := proj.Resolve(indexPath, base)
	if err != nil {
		return lsprecall.Cell{}, fmt.Errorf("resolve: %w", err)
	}
	if _, statErr := os.Stat(p.DBPath); statErr != nil {
		if cfgErr := ensureCorpusIndexConfig(indexPath); cfgErr != nil {
			return lsprecall.Cell{}, fmt.Errorf("seed index config: %w", cfgErr)
		}
		fmt.Fprintf(os.Stderr, "dex bench lsp: graph-indexing %s (%s)\n", spec.Name, indexPath)
		if ixErr := cmdIndex(ctx, []string{"--graph=only", indexPath}); ixErr != nil {
			return lsprecall.Cell{}, fmt.Errorf("graph-index: %w", ixErr)
		}
	}

	view, err := openTraceView(ctx, p.DBPath)
	if err != nil {
		return lsprecall.Cell{}, err
	}

	lang, lspCmd, goldPaths, err := lspSpec(spec, tsCmd)
	if err != nil {
		return lsprecall.Cell{}, err
	}

	var allProbes []lsprecall.ProbeResult
	for _, gp := range goldPaths {
		gold, err := trace.LoadGold(gp)
		if err != nil {
			return lsprecall.Cell{}, fmt.Errorf("load gold %s: %w", gp, err)
		}
		fmt.Fprintf(os.Stderr, "dex bench lsp: running %d probe(s) for %s (%s)\n", len(gold.Probes), spec.Name, gp)
		probes, err := lsprecall.RunProbes(ctx, gold, dir, view, lspCmd, lang, timeout)
		if err != nil {
			return lsprecall.Cell{}, fmt.Errorf("run probes: %w", err)
		}
		allProbes = append(allProbes, probes...)
	}
	return lsprecall.BuildCell(spec.Name, lang, allProbes), nil
}

func lspProject(ctx context.Context, base, project, goldFlag string, tsCmd []string, timeout time.Duration) ([]lsprecall.Cell, error) {
	if goldFlag == "" {
		return nil, fmt.Errorf("dex bench lsp: --project requires --gold")
	}
	p, err := proj.Resolve(project, base)
	if err != nil {
		return nil, fmt.Errorf("dex bench lsp: resolve project: %w", err)
	}
	if _, statErr := os.Stat(p.DBPath); statErr != nil {
		fmt.Fprintf(os.Stderr, "dex bench lsp: graph-indexing %s\n", project)
		if ixErr := cmdIndex(ctx, []string{"--graph=only", project}); ixErr != nil {
			return nil, fmt.Errorf("dex bench lsp: graph-index: %w", ixErr)
		}
	}
	view, err := openTraceView(ctx, p.DBPath)
	if err != nil {
		return nil, err
	}
	gold, err := trace.LoadGold(goldFlag)
	if err != nil {
		return nil, fmt.Errorf("dex bench lsp: load gold: %w", err)
	}

	lang := lspLangFromGold(gold)
	lspCmd, err := lspCmdForLang(lang, tsCmd)
	if err != nil {
		return nil, fmt.Errorf("dex bench lsp: %w", err)
	}

	absProject, err := filepath.Abs(project)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(os.Stderr, "dex bench lsp: running %d probe(s) for %s\n", len(gold.Probes), gold.Repo)
	probes, err := lsprecall.RunProbes(ctx, gold, absProject, view, lspCmd, lang, timeout)
	if err != nil {
		return nil, fmt.Errorf("dex bench lsp: %w", err)
	}
	label := filepath.Base(strings.TrimRight(project, string(os.PathSeparator)))
	return []lsprecall.Cell{lsprecall.BuildCell(label, lang, probes)}, nil
}

// lspSpec returns the language ID, LSP command, and gold paths for a corpus repo.
func lspSpec(spec corpus.RepoSpec, tsCmd []string) (lang string, lspCmd []string, goldPaths []string, err error) {
	// Pick the first non-Go language the repo declares — that's what we want LSP for.
	for _, l := range spec.Languages {
		switch l {
		case "ts", "typescript", "js", "javascript":
			lang = "typescript"
			lspCmd = tsCmd
		case "java":
			lang = "java"
			lspCmd = []string{"jdtls"} // placeholder; jdtls not yet installed
		}
		if lang != "" {
			break
		}
	}
	if lang == "" {
		return "", nil, nil, fmt.Errorf("no supported non-Go language in repo %s (languages: %v)", spec.Name, spec.Languages)
	}
	for _, ts := range spec.TraceSets {
		abs, err := filepath.Abs(ts)
		if err != nil {
			return "", nil, nil, fmt.Errorf("resolve trace set %s: %w", ts, err)
		}
		goldPaths = append(goldPaths, abs)
	}
	if len(goldPaths) == 0 {
		return "", nil, nil, fmt.Errorf("repo %s has no trace_sets to use as gold", spec.Name)
	}
	return lang, lspCmd, goldPaths, nil
}

func lspLangFromGold(gold trace.Gold) string {
	switch gold.Lang {
	case "ts", "typescript", "js", "javascript":
		return "typescript"
	case "java":
		return "java"
	default:
		return gold.Lang
	}
}

func lspCmdForLang(lang string, tsCmd []string) ([]string, error) {
	switch lang {
	case "typescript":
		return tsCmd, nil
	case "java":
		return []string{"jdtls"}, nil
	default:
		return nil, fmt.Errorf("no LSP server configured for language %q", lang)
	}
}
