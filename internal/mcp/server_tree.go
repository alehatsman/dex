package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type SearchTreeInput struct {
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project or git worktree you are working in. The server cannot see your shell's directory; when working in a worktree different from where the server started, pass that worktree's path"`
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
	Text        string      `json:"text,omitempty"` // compact text tree for direct consumption
}

// SearchTree is the exported entry point used by CLI subcommands.
func (s *Server) SearchTree(ctx context.Context, in SearchTreeInput) (SearchTreeOutput, error) {
	_, out, err := s.searchTree(ctx, nil, in)
	return out, err
}

func (s *Server) searchTree(ctx context.Context, _ *sdk.CallToolRequest, in SearchTreeInput) (*sdk.CallToolResult, SearchTreeOutput, error) {
	p, hint := s.resolveProject(ctx, in.ProjectRoot)
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

	st, err := s.openStore(p.DBPath)
	if err != nil {
		return nil, SearchTreeOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
	}

	// Validate that the requested subdirectory actually exists on disk before
	// querying the index, so callers get not-found instead of a misleading
	// ok+empty response when the path was never indexed or doesn't exist.
	if prefix != "" {
		info, statErr := os.Stat(filepath.Join(p.Root, prefix))
		if errors.Is(statErr, os.ErrNotExist) {
			return nil, SearchTreeOutput{
				Status: "not-found",
				Root:   p.Root,
				Hint:   fmt.Sprintf("path %q does not exist under %s", prefix, p.Root),
			}, nil
		}
		if statErr == nil && !info.IsDir() {
			return nil, SearchTreeOutput{
				Status: "error",
				Root:   p.Root,
				Hint:   fmt.Sprintf("path %q is a file, not a directory — ls lists directories", prefix),
			}, nil
		}
	}

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

	out := SearchTreeOutput{
		Status:      "ok",
		Root:        p.Root,
		Path:        in.Path,
		Entries:     entries,
		TotalFiles:  len(files),
		TotalChunks: totalChunks,
		Text:        renderTreeText(entries, in.Path, len(files), totalChunks),
	}
	return nil, out, nil
}

// renderTreeText produces a compact indented text representation of the tree.
func renderTreeText(entries []TreeEntry, rootPath string, totalFiles, totalChunks int) string {
	var b strings.Builder
	header := rootPath
	if header == "" || header == "." {
		header = "."
	}
	fmt.Fprintf(&b, "%s  (%d files, %d chunks)\n", header, totalFiles, totalChunks)
	for _, e := range entries {
		name := e.Path
		// Show just the last component for readability.
		if idx := strings.LastIndex(strings.TrimRight(name, "/"), "/"); idx >= 0 {
			name = name[idx+1:]
		}
		if strings.HasSuffix(e.Path, "/") {
			fmt.Fprintf(&b, "  %s  (%dc)\n", name, e.Chunks)
		} else {
			fmt.Fprintf(&b, "  %-40s %dc\n", name, e.Chunks)
		}
	}
	return b.String()
}
