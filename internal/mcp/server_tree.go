package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/alehatsman/dex/internal/store"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type SearchTreeInput struct {
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
	Path        string `json:"path,omitempty"         jsonschema:"relative directory within the project to list (default: project root)"`
	Depth       int    `json:"depth,omitempty"        jsonschema:"max directory depth shown individually; dirs deeper than this are aggregated with their chunk totals (default 3, 0 = unlimited)"`
}

// TreeEntry is one row in a SearchTreeOutput — either an individual indexed
// file or an aggregated directory. Dirs are distinguishable by a trailing /.
type TreeEntry struct {
	Path   string `json:"path"`   // relative to project root; dirs end with /
	Chunks int    `json:"chunks"` // indexed chunk count
}

type SearchTreeOutput struct {
	Status      string      `json:"status"` // "ok" | "no-index" | "not-found" | "error"
	Hint        string      `json:"hint,omitempty"`
	Root        string      `json:"root,omitempty"`
	Path        string      `json:"path,omitempty"`
	Entries     []TreeEntry `json:"entries,omitempty"`
	TotalFiles  int         `json:"total_files"`
	TotalChunks int         `json:"total_chunks"`
}

// SearchTree is the exported entry point used by CLI subcommands.
func (s *Server) SearchTree(ctx context.Context, in SearchTreeInput) (SearchTreeOutput, error) {
	_, out, err := s.searchTree(ctx, nil, in)
	return out, err
}

func (s *Server) searchTree(ctx context.Context, _ *sdk.CallToolRequest, in SearchTreeInput) (*sdk.CallToolResult, SearchTreeOutput, error) {
	p, hint := s.resolveProject(in.ProjectRoot)
	if hint != "" {
		return nil, SearchTreeOutput{Status: "error", Hint: hint}, nil
	}
	if _, err := os.Stat(p.DBPath); errors.Is(err, os.ErrNotExist) {
		return nil, SearchTreeOutput{
			Status: "no-index",
			Hint:   fmt.Sprintf("no index for %s — run `dex index %s` first.", p.Root, p.Root),
		}, nil
	}

	prefix := strings.Trim(in.Path, "/")
	if prefix == "." {
		prefix = ""
	}

	depth := max(in.Depth, 0)
	if depth == 0 {
		depth = 3
	}

	st, err := store.OpenWith(ctx, p.DBPath, s.StoreOpts)
	if err != nil {
		return nil, SearchTreeOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
	}
	defer func() { _ = st.Close() }()

	files, err := st.FileTree(ctx, prefix)
	if err != nil {
		return nil, SearchTreeOutput{Status: "error", Hint: fmt.Sprintf("file tree: %v", err)}, nil
	}

	// Separate shallow files from deep ones; aggregate the deep ones by dir.
	dirChunks := map[string]int{}
	var entries []TreeEntry
	totalChunks := 0
	for _, f := range files {
		totalChunks += f.Chunks
		rel := f.Path
		if prefix != "" {
			rel = strings.TrimPrefix(f.Path, prefix+"/")
		}
		parts := strings.Split(rel, "/")
		if len(parts) <= depth {
			entries = append(entries, TreeEntry{Path: f.Path, Chunks: f.Chunks})
		} else {
			dirRel := strings.Join(parts[:depth], "/")
			var dirPath string
			if prefix != "" {
				dirPath = prefix + "/" + dirRel + "/"
			} else {
				dirPath = dirRel + "/"
			}
			dirChunks[dirPath] += f.Chunks
		}
	}
	for dir, chunks := range dirChunks {
		entries = append(entries, TreeEntry{Path: dir, Chunks: chunks})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })

	return nil, SearchTreeOutput{
		Status:      "ok",
		Root:        p.Root,
		Path:        in.Path,
		Entries:     entries,
		TotalFiles:  len(files),
		TotalChunks: totalChunks,
	}, nil
}
