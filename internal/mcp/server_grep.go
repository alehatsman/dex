package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/alehatsman/dex/internal/throttle"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type SearchGrepInput struct {
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
	Pattern     string `json:"pattern"                jsonschema:"RE2 regex pattern to search for"`
	Path        string `json:"path,omitempty"         jsonschema:"relative subdirectory to restrict the search (default: project root)"`
	Ext         string `json:"ext,omitempty"          jsonschema:"file extension filter without leading dot, e.g. go or ts"`
	MaxResults  int    `json:"max_results,omitempty"  jsonschema:"maximum matches to return (default 50, max 200)"`
	Context     int    `json:"context,omitempty"      jsonschema:"lines of surrounding context to include before AND after each match (like grep -C), 0-10; default 0"`
}

type GrepMatch struct {
	Path    string `json:"path"`    // relative to project root
	Line    int    `json:"line"`    // 1-indexed
	Content string `json:"content"` // trimmed match line
	// Before/After are up to `context` raw lines surrounding the match (#662),
	// indentation preserved so the snippet reads naturally. Empty when context=0.
	Before []string `json:"before,omitempty"`
	After  []string `json:"after,omitempty"`
}

type SearchGrepOutput struct {
	Status    string      `json:"status"` // "ok" | "no-matches" | "not-found" | "error"
	Hint      string      `json:"hint,omitempty"`
	Project   string      `json:"project,omitempty"`
	Matches   []GrepMatch `json:"matches,omitempty"`
	Total     int         `json:"total"`
	Truncated bool        `json:"truncated,omitempty"`
}

// SearchGrep is the exported entry point used by CLI subcommands.
func (s *Server) SearchGrep(ctx context.Context, in SearchGrepInput) (SearchGrepOutput, error) {
	_, out, err := s.searchGrep(ctx, nil, in)
	return out, err
}

func (s *Server) searchGrep(ctx context.Context, _ *sdk.CallToolRequest, in SearchGrepInput) (*sdk.CallToolResult, SearchGrepOutput, error) { //nolint:cyclop
	p, hint := s.resolveProject(in.ProjectRoot)
	if hint != "" {
		return nil, SearchGrepOutput{Status: "error", Hint: hint}, nil
	}
	if in.Pattern == "" {
		return nil, SearchGrepOutput{Status: "error", Hint: "pattern is required"}, nil
	}
	re, err := regexp.Compile(in.Pattern)
	if err != nil {
		return nil, SearchGrepOutput{Status: "error", Hint: fmt.Sprintf("invalid pattern: %v", err)}, nil
	}

	maxResults := in.MaxResults
	if maxResults <= 0 || maxResults > 200 {
		maxResults = 50
	}
	ctxN := in.Context
	if ctxN < 0 {
		ctxN = 0
	}
	if ctxN > 10 {
		ctxN = 10
	}

	prefix := strings.Trim(in.Path, "/")
	if prefix == "." {
		prefix = ""
	}
	// Validate an explicit path exists before searching — otherwise a typo'd
	// path silently falls through to walking the whole project root and returns
	// a misleading "no-matches". Accept both directories and a single file (the
	// `rg pattern file.go` idiom, and what sibling tools accept); reject only
	// genuinely missing paths, and say so honestly.
	var prefixIsFile bool
	if prefix != "" {
		info, statErr := os.Stat(filepath.Join(p.Root, prefix))
		if statErr != nil {
			return nil, SearchGrepOutput{Status: "not-found", Project: p.Root,
				Hint: fmt.Sprintf("path %q does not exist under %s", prefix, p.Root)}, nil
		}
		prefixIsFile = !info.IsDir()
	}
	extFilter := strings.TrimPrefix(in.Ext, ".")

	// Build the file list. A single-file path scopes the scan to exactly that
	// file (an ext filter excluding it yields an empty set → no-matches, never a
	// whole-repo walk); a directory uses the index when available and falls back
	// to walking the fs.
	var filePaths []string
	if prefixIsFile {
		if extFilter == "" || strings.HasSuffix(prefix, "."+extFilter) {
			filePaths = append(filePaths, filepath.Join(p.Root, prefix))
		}
	} else if _, statErr := os.Stat(p.DBPath); !errors.Is(statErr, os.ErrNotExist) {
		st, openErr := s.openStore(p.DBPath)
		if openErr == nil {
			if files, treeErr := st.FileTree(ctx, prefix); treeErr == nil {
				for _, f := range files {
					if extFilter != "" && !strings.HasSuffix(f.Path, "."+extFilter) {
						continue
					}
					filePaths = append(filePaths, filepath.Join(p.Root, f.Path))
				}
			}
		}
	}
	if !prefixIsFile && len(filePaths) == 0 {
		searchRoot := p.Root
		if prefix != "" {
			searchRoot = filepath.Join(p.Root, prefix)
		}
		if err := filepath.Walk(searchRoot, func(path string, info os.FileInfo, walkErr error) error {
			if walkErr != nil {
				if path == searchRoot {
					return walkErr // propagate root-level errors (e.g. path doesn't exist)
				}
				return nil // skip inaccessible subdirectories
			}
			if info.IsDir() {
				switch info.Name() {
				case ".git", "vendor", "node_modules", ".dex":
					return filepath.SkipDir
				}
				return nil
			}
			if extFilter != "" && !strings.HasSuffix(path, "."+extFilter) {
				return nil
			}
			filePaths = append(filePaths, path)
			return nil
		}); err != nil {
			return nil, SearchGrepOutput{Status: "not-found", Hint: fmt.Sprintf("cannot walk %s: %v", searchRoot, err)}, nil
		}
	}

	// Narrow the candidate set using the trigram index before reading files.
	// Falls back to the full filePaths list when the pattern has no word
	// trigrams (pure regex metacharacters) or the index cannot narrow.
	if len(filePaths) > 0 {
		tgKey := trigramCacheKey{root: p.Root, prefix: prefix, ext: extFilter}
		idx := s.tgCache.getOrBuild(tgKey, filePaths)
		if narrowed, ok := idx.Narrow(in.Pattern); ok {
			filePaths = narrowed
		}
	}

	var matches []GrepMatch
	truncated := false
outer:
	for _, absPath := range filePaths {
		data, readErr := os.ReadFile(absPath)
		if readErr != nil {
			continue
		}
		relPath, _ := filepath.Rel(p.Root, absPath)
		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			if re.MatchString(line) {
				matches = append(matches, newGrepMatch(lines, i, relPath, ctxN))
				if len(matches) >= maxResults {
					truncated = true
					break outer
				}
			}
		}
	}

	// grep is a deterministic local RE2 scan with no embedding/GPU/chat cost,
	// so it is not search-class: it must not consume (or be blocked by) the
	// shared search-group budget that protects find/ask (#513). The scan above
	// has also already run by the time we get here, so blocking would save
	// nothing and an empty loop-blocked payload is indistinguishable from a
	// genuine no-matches result. Never suppress grep results — at most trim and
	// surface the loop-detector hint as advisory guidance.
	ldLevel, ldHint := s.ld().Check("grep", throttle.ArgsKey(in.Pattern), false)

	status := "ok"
	if len(matches) == 0 {
		status = "no-matches"
	}
	out := SearchGrepOutput{
		Status:    status,
		Project:   p.Root,
		Matches:   matches,
		Total:     len(matches),
		Truncated: truncated,
	}
	if truncated {
		out.Hint = fmt.Sprintf("results capped at %d — narrow the pattern or path to see more", maxResults)
	}
	if (ldLevel == throttle.Reduce || ldLevel == throttle.Block) && len(out.Matches) > 10 {
		out.Matches = out.Matches[:10]
		out.Total = 10
		out.Hint = ldHint + " [reduced: showing top 10]"
	} else if ldHint != "" && out.Hint == "" {
		out.Hint = ldHint
	}
	return nil, out, nil
}

// newGrepMatch builds a match for the line at index i, attaching up to ctxN
// surrounding lines before and after (#662). lines is the file's split content.
func newGrepMatch(lines []string, i int, relPath string, ctxN int) GrepMatch {
	m := GrepMatch{Path: relPath, Line: i + 1, Content: strings.TrimSpace(lines[i])}
	if ctxN > 0 {
		m.Before = grepContextSlice(lines, i-ctxN, i)
		m.After = grepContextSlice(lines, i+1, i+1+ctxN)
	}
	return m
}

// grepContextSlice returns lines[lo:hi] clamped to the file bounds, with a
// single trailing empty element (the artifact of splitting a trailing newline)
// dropped so EOF context doesn't show a phantom blank line.
func grepContextSlice(lines []string, lo, hi int) []string {
	if lo < 0 {
		lo = 0
	}
	if hi > len(lines) {
		hi = len(lines)
	}
	if lo >= hi {
		return nil
	}
	seg := lines[lo:hi]
	if n := len(seg); n > 0 && seg[n-1] == "" && hi == len(lines) {
		seg = seg[:n-1]
	}
	if len(seg) == 0 {
		return nil
	}
	return append([]string(nil), seg...)
}
