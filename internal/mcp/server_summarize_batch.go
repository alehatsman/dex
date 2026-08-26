package mcp

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/alehatsman/dex/internal/compress"
	"github.com/alehatsman/dex/internal/profiles"
	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/store"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// summarizeBatch handles file_view when paths[] is provided.
// All files are processed with the same mode in a single call.
// Go import blocks are deduplicated across files (union emitted once as a shared
// header; per-file blocks replaced with a back-reference) unless dedup=false.
// When 3+ files are successfully read, a TF-IDF codebook is also applied to
// replace any remaining repeated lines with short §N refs.
func (s *Server) summarizeBatch(ctx context.Context, in SummarizeInput) (*sdk.CallToolResult, SummarizeOutput, error) {
	const maxBatch = 10
	if len(in.Paths) > maxBatch {
		return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("batch too large: max %d files per call, got %d", maxBatch, len(in.Paths))}, nil
	}
	mode := strings.ToLower(strings.TrimSpace(in.Mode))
	if mode == "" {
		mode = string(ReadModeSignatures)
	}

	// Stable-first layout: load session-stable file set before the per-file
	// loop so we can annotate results as they come in.
	stableSet := batchStableSet(ctx, in.ProjectRoot, s.IndexDir)

	type fileResult struct {
		path    string // resolved path (or "" for errors)
		header  string // "## <path>" or "## <rawPath>\n⚠ <hint>"
		content string // file content (empty for errors)
		ok      bool
		stable  bool // session-seen before this turn
	}

	results := make([]fileResult, 0, len(in.Paths))
	var project string

	for _, rawPath := range in.Paths {
		single := in
		single.Path = rawPath
		single.Paths = nil
		single.Mode = mode
		_, out, err := s.summarize(ctx, nil, single)
		if err != nil {
			return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("%s: %v", rawPath, err)}, nil
		}
		if project == "" {
			project = out.Project
		}
		if out.Status != "ok" {
			results = append(results, fileResult{
				header: fmt.Sprintf("## %s\n⚠ %s", rawPath, out.Hint),
			})
			continue
		}
		results = append(results, fileResult{
			path:    out.Path,
			header:  fmt.Sprintf("## %s", out.Path),
			content: out.Content,
			ok:      true,
			stable:  stableSet[out.Path],
		})
	}

	// cache_layout=stable_first (default): move session-stable files to the
	// front so the Anthropic prompt cache can build a consistent prefix across
	// turns. Preserve relative order within each tier. No-op when no session
	// exists, only one file, or the profile opts out.
	// Per-call override wins; fall back to profile; fall back to stable_first.
	layout := in.CacheLayout
	if layout == "" {
		layout = profiles.Active(in.ProjectRoot).Read.CacheLayout
	}
	if layout == "" {
		layout = "stable_first" // default
	}
	if layout == "stable_first" && len(results) > 1 {
		stable := results[:0:0]
		fresh := results[:0:0]
		for _, r := range results {
			if r.ok && r.stable {
				stable = append(stable, r)
			} else {
				fresh = append(fresh, r)
			}
		}
		results = append(stable, fresh...)
	}

	// Build file-content slice for dedup + codebook.
	var fileContents []string
	for _, r := range results {
		if r.ok {
			fileContents = append(fileContents, r.content)
		}
	}

	// Go import block deduplication: extract the parenthesized import block from
	// each file, emit the union once as a shared header, replace per-file blocks
	// with a single back-reference line. Applied when: mode is full or the content
	// happens to contain import blocks (dedup is a no-op when no blocks are found),
	// and the caller has not opted out with dedup=false.
	var dedupHeader string
	var importsDedupSavedLines int
	if in.Dedup == nil || *in.Dedup {
		var deduped []string
		dedupHeader, deduped, importsDedupSavedLines = compress.DeduplicateGoImports(fileContents)
		if dedupHeader != "" {
			// Splice deduped content back into results.
			j := 0
			for i := range results {
				if results[i].ok {
					results[i].content = deduped[j]
					j++
				}
			}
			fileContents = deduped
		}
	}

	cb := compress.BuildCodebook(fileContents)

	var sb strings.Builder
	if dedupHeader != "" {
		sb.WriteString(dedupHeader)
		sb.WriteByte('\n')
	}
	if !cb.Empty() {
		sb.WriteString(cb.Legend())
		sb.WriteByte('\n')
	}

	var resolvedPaths []string
	var stablePrefixTokens int
	countingStable := layout == "stable_first"
	for _, r := range results {
		if r.ok {
			content := cb.Apply(r.content)
			chunk := fmt.Sprintf("%s\n%s\n\n", r.header, content)
			sb.WriteString(chunk)
			resolvedPaths = append(resolvedPaths, r.path)
			if countingStable && r.stable {
				stablePrefixTokens += compress.EstimateTokens(chunk)
			} else {
				countingStable = false // stop counting once we hit a fresh file
			}
		} else {
			fmt.Fprintf(&sb, "%s\n\n", r.header)
			if countingStable {
				countingStable = false
			}
		}
	}

	return nil, SummarizeOutput{
		Status:                 "ok",
		Project:                project,
		Content:                strings.TrimRight(sb.String(), "\n"),
		Paths:                  resolvedPaths,
		StablePrefixTokens:     stablePrefixTokens,
		ImportsDedupSavedLines: importsDedupSavedLines,
	}, nil
}

// batchStableSet returns the set of project-relative file paths that appear
// in the current session — these are "session-stable" for cache layout
// purposes. Returns an empty (non-nil) map when no session exists or on any
// error (failures are silently swallowed; this is a best-effort optimisation).
func batchStableSet(ctx context.Context, projectRoot, indexDir string) map[string]bool {
	root := projectRoot
	if root == "" {
		var err error
		root, err = os.Getwd()
		if err != nil {
			return map[string]bool{}
		}
	}
	p, err := proj.Resolve(root, indexDir)
	if err != nil {
		return map[string]bool{}
	}
	st, err := store.Open(ctx, p.DBPath)
	if err != nil {
		return map[string]bool{}
	}
	defer st.Close()
	ss, ok, err := st.SessionGet(ctx)
	if err != nil || !ok {
		return map[string]bool{}
	}
	set := make(map[string]bool, len(ss.Files))
	for _, f := range ss.Files {
		set[f.Path] = true
	}
	return set
}

// Pure line-slicing and index-sourced formatting helpers (parseLinesRange,
// formatSignatures, formatMap, sliceLines, buildSummarizeSystem) moved to
// internal/summarize (#472 step 3).
