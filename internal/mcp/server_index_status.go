package mcp

import (
	"context"
	"os"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// IndexStatusInput is the input to the index_status tool.
type IndexStatusInput struct {
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
}

// IndexStatusOutput is the output of the index_status tool.
type IndexStatusOutput struct {
	Status        string   `json:"status"` // "ok" | "no-index" | "error"
	Hint          string   `json:"hint,omitempty"`
	Fresh         bool     `json:"fresh"`
	LastIndexedAt string   `json:"last_indexed_at,omitempty"`
	WatchActive   bool     `json:"watch_active"`
	Warnings      []string `json:"warnings,omitempty"`
}

// indexStatus checks whether the project index exists and is fresh.
func (s *Server) indexStatus(_ context.Context, _ *sdk.CallToolRequest, in IndexStatusInput) (*sdk.CallToolResult, IndexStatusOutput, error) {
	p, hint := s.resolveProject(in.ProjectRoot)
	if hint != "" {
		return nil, IndexStatusOutput{Status: "error", Hint: hint}, nil
	}

	info, err := os.Stat(p.DBPath)
	if os.IsNotExist(err) {
		return nil, IndexStatusOutput{
			Status: "no-index",
			Hint:   "run dex index " + p.Root,
		}, nil
	}
	if err != nil {
		return nil, IndexStatusOutput{Status: "error", Hint: err.Error()}, nil
	}

	// A watcher is active when the project's watcher entry is a struct{} (running marker).
	watchActive := s.watcherActive(p.ID)

	// "fresh" = indexed within last 5 minutes or a watcher is active.
	fresh := watchActive || time.Since(info.ModTime()) < 5*time.Minute

	return nil, IndexStatusOutput{
		Status:        "ok",
		Fresh:         fresh,
		LastIndexedAt: info.ModTime().UTC().Format(time.RFC3339),
		WatchActive:   watchActive,
	}, nil
}

// watcherActive returns true when the project (by ID) has an active watcher goroutine.
func (s *Server) watcherActive(projectID string) bool {
	val, ok := s.watchers.Load(projectID)
	if !ok {
		return false
	}
	_, isRunning := val.(struct{})
	return isRunning
}
