package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/alehatsman/dex/internal/chat"
	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/store"
	"github.com/alehatsman/dex/internal/summarize"
)

// summaryPromptVersion is baked into every stored summary. Bump it whenever the
// prompt or input assembly below changes meaning — existing rows then read as
// stale and `dex summarize` regenerates them. (#572)
const summaryPromptVersion = 1

// maxSummaryInputBytes caps the chunk-body text sent to the model per file.
const maxSummaryInputBytes = 48 * 1024

// cmdSummarize generates per-file LLM summaries into the index. It is an
// isolated derived artifact: it only reads chunk bodies already in sqlite and
// writes the file_summaries table — no retrieval/fusion path is touched.
func cmdSummarize(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("summarize", flag.ContinueOnError)
	setHelp(fs,
		"Generate per-file LLM summaries into the index (isolated from search).",
		"dex summarize [flags] [path...]",
		"dex summarize                 # summarize the whole index (stale files only)",
		"dex summarize internal/store  # only the given paths",
		"dex summarize --get FILE      # print a stored summary as JSON")
	get := fs.String("get", "", "print the stored summary for a path as JSON, without generating")
	force := fs.Bool("force", false, "regenerate even when the source hash is unchanged")
	focus := fs.String("focus", "", "optional focus hint passed to the summarizer")
	format := fs.String("format", "text", "output format: text|json")
	verbose := fs.Bool("v", false, "verbose: print each summarized path")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	switch *format {
	case "text", "json":
	default:
		return fmt.Errorf("unknown --format=%s (want text|json)", *format)
	}

	base, err := indexDir()
	if err != nil {
		return err
	}
	p, err := proj.Resolve(".", base)
	if err != nil {
		return err
	}
	st, err := openStore(ctx, p.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	if *get != "" {
		return runSummaryGet(ctx, st, toRelPath(p.Root, *get), *format)
	}
	return runSummaryGenerate(ctx, st, p.Root, fs.Args(), *focus, *force, *verbose, *format)
}

// runSummaryGet prints a stored summary (or a not-found notice) for one path.
func runSummaryGet(ctx context.Context, st *store.Store, relPath, format string) error {
	sum, ok, err := st.GetFileSummary(ctx, relPath)
	if err != nil {
		return err
	}
	if format == "json" {
		out := map[string]any{"path": relPath, "found": ok}
		if ok {
			out["summary"] = sum.Summary
			out["model"] = sum.Model
			out["prompt_version"] = sum.PromptVersion
			out["generated_at"] = sum.GeneratedAt.UTC().Format(time.RFC3339)
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	if !ok {
		fmt.Printf("no summary for %s (run: dex summarize %s)\n", relPath, relPath)
		return nil
	}
	fmt.Println(sum.Summary)
	return nil
}

// runSummaryGenerate (re)generates summaries for the target paths, or for the
// whole index when none are given. Stale-only by default (source-hash gate).
func runSummaryGenerate(ctx context.Context, st *store.Store, root string, targets []string, focus string, force, verbose bool, format string) error {
	paths, err := summaryTargetPaths(ctx, st, root, targets)
	if err != nil {
		return err
	}
	client := newChatClient()
	system := summarize.BuildSystem(focus)

	var done, skipped, failed int
	for _, rel := range paths {
		bodies, err := st.ChunkBodiesByPath(ctx, rel)
		if err != nil {
			return err
		}
		if len(bodies) == 0 {
			skipped++ // nothing indexed for this path (e.g. ignored/binary)
			continue
		}
		srcHash := store.FileBodyHash(bodies)
		if !force {
			h, pv, ok, err := st.FileSummaryMeta(ctx, rel)
			if err != nil {
				return err
			}
			if ok && pv == summaryPromptVersion && h == srcHash {
				skipped++
				continue
			}
		}

		msgs := []chat.Message{
			{Role: "system", Content: system},
			{Role: "user", Content: buildSummaryInput(rel, bodies)},
		}
		resp, err := client.Generate(ctx, msgs, chat.Options{})
		if err != nil {
			if errors.Is(err, chat.ErrUnreachable) {
				return fmt.Errorf("chat service offline (%w) — start it or set DEX_CHAT_URL", err)
			}
			fmt.Fprintf(os.Stderr, "dex: summarize %s failed: %v\n", rel, err)
			failed++
			continue
		}
		if err := st.UpsertFileSummary(ctx, store.FileSummary{
			Path:          rel,
			SourceHash:    srcHash,
			PromptVersion: summaryPromptVersion,
			Model:         client.ModelName(),
			Summary:       strings.TrimSpace(resp.Content),
			GeneratedAt:   time.Now(),
		}); err != nil {
			return err
		}
		done++
		if verbose {
			fmt.Printf("summarized %s\n", rel)
		}
	}

	if format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]int{
			"generated": done, "skipped": skipped, "failed": failed, "considered": len(paths),
		})
	}
	fmt.Printf("summarize: %d generated, %d up-to-date, %d failed (%d files)\n",
		done, skipped, failed, len(paths))
	return nil
}

// summaryTargetPaths resolves the file set to summarize: the given paths
// (normalized to index-relative form), or every indexed file when none given.
func summaryTargetPaths(ctx context.Context, st *store.Store, root string, targets []string) ([]string, error) {
	if len(targets) > 0 {
		paths := make([]string, 0, len(targets))
		for _, t := range targets {
			paths = append(paths, toRelPath(root, t))
		}
		return paths, nil
	}
	m, err := st.CodeFilePaths(ctx)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(m))
	for path := range m {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths, nil
}

// buildSummaryInput assembles the model input from a file's chunk bodies — the
// canonical text dex already holds, read straight from sqlite. Bodies arrive
// ordered by start line; the total is capped at maxSummaryInputBytes.
func buildSummaryInput(relPath string, bodies []store.ChunkBody) string {
	var b strings.Builder
	fmt.Fprintf(&b, "FILE: %s\n\n", relPath)
	for _, body := range bodies {
		if b.Len() >= maxSummaryInputBytes {
			b.WriteString("\n[...truncated...]\n")
			break
		}
		fmt.Fprintf(&b, "```\n%s\n```\n", strings.TrimRight(body.Content, "\n"))
	}
	return b.String()
}

// toRelPath normalizes a CLI path argument to the index-relative form stored in
// the index (paths are kept relative to the project root). Non-rooted or
// already-relative inputs pass through cleaned.
func toRelPath(root, path string) string {
	abs := path
	if !filepath.IsAbs(abs) {
		if cwd, err := os.Getwd(); err == nil {
			abs = filepath.Join(cwd, path)
		}
	}
	if rel, err := filepath.Rel(root, abs); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(filepath.Clean(path))
}
