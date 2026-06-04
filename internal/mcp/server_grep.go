package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/alehatsman/dex/internal/store"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type SearchGrepInput struct {
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
	Pattern     string `json:"pattern"                jsonschema:"RE2 regex pattern to search for"`
	Path        string `json:"path,omitempty"         jsonschema:"relative subdirectory to restrict the search (default: project root)"`
	Ext         string `json:"ext,omitempty"          jsonschema:"file extension filter without leading dot, e.g. go or ts"`
	MaxResults  int    `json:"max_results,omitempty"  jsonschema:"maximum matches to return (default 50, max 200)"`
}

type GrepMatch struct {
	Path    string `json:"path"`    // relative to project root
	Line    int    `json:"line"`    // 1-indexed
	Content string `json:"content"` // trimmed match line
}

type SearchGrepOutput struct {
	Status    string      `json:"status"` // "ok" | "no-matches" | "error"
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

func (s *Server) searchGrep(ctx context.Context, _ *sdk.CallToolRequest, in SearchGrepInput) (*sdk.CallToolResult, SearchGrepOutput, error) {
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

	prefix := strings.Trim(in.Path, "/")
	if prefix == "." {
		prefix = ""
	}
	extFilter := strings.TrimPrefix(in.Ext, ".")

	// Build file list from the index when available; fall back to walking the fs.
	var filePaths []string
	if _, statErr := os.Stat(p.DBPath); !errors.Is(statErr, os.ErrNotExist) {
		st, openErr := store.OpenWith(ctx, p.DBPath, s.StoreOpts)
		if openErr == nil {
			defer func() { _ = st.Close() }()
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
	if len(filePaths) == 0 {
		searchRoot := p.Root
		if prefix != "" {
			searchRoot = filepath.Join(p.Root, prefix)
		}
		_ = filepath.Walk(searchRoot, func(path string, info os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return nil
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
		})
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
		for i, line := range strings.Split(string(data), "\n") {
			if re.MatchString(line) {
				matches = append(matches, GrepMatch{
					Path:    relPath,
					Line:    i + 1,
					Content: strings.TrimSpace(line),
				})
				if len(matches) >= maxResults {
					truncated = true
					break outer
				}
			}
		}
	}

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
	return nil, out, nil
}
