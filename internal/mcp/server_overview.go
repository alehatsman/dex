package mcp

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"

	"github.com/alehatsman/dex/internal/embed"
	"github.com/alehatsman/dex/internal/store"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type OverviewInput struct {
	Task        string `json:"task" jsonschema:"task description used to rank files by relevance"`
	K           int    `json:"k,omitempty" jsonschema:"number of context files to surface (default 8, max 20)"`
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
}

// OverviewFile is one file in the context set, ranked by relevance to the task.
type OverviewFile struct {
	Path          string  `json:"path"`
	Lines         int     `json:"lines,omitempty"`
	Score         float64 `json:"score"`
	SuggestedMode string  `json:"suggested_mode"` // "full" | "signatures" | "map"
}

type OverviewOutput struct {
	Status       string         `json:"status"` // "ok" | "no-index" | "embedding-service-unreachable" | "error"
	Hint         string         `json:"hint,omitempty"`
	Project      string         `json:"project,omitempty"`
	Task         string         `json:"task,omitempty"`
	Context      []OverviewFile `json:"context,omitempty"`
	DistantCount int            `json:"distant_count"`
	Distant      []string       `json:"distant,omitempty"`
}

func (s *Server) Overview(ctx context.Context, in OverviewInput) (OverviewOutput, error) {
	_, out, err := s.overview(ctx, nil, in)
	return out, err
}

func (s *Server) overview(ctx context.Context, _ *sdk.CallToolRequest, in OverviewInput) (*sdk.CallToolResult, OverviewOutput, error) {
	if in.Task == "" {
		return nil, OverviewOutput{Status: "error", Hint: "task is empty"}, nil
	}
	p, hint := s.resolveProject(in.ProjectRoot)
	if hint != "" {
		return nil, OverviewOutput{Status: "error", Hint: hint}, nil
	}
	if _, err := os.Stat(p.DBPath); errors.Is(err, os.ErrNotExist) {
		return nil, OverviewOutput{Status: "no-index", Project: p.Root,
			Hint: fmt.Sprintf("no index for %s — run `dex index %s` first.", p.Root, p.Root)}, nil
	}

	k := in.K
	if k <= 0 {
		k = 8
	}
	if k > 20 {
		k = 20
	}

	// Embed the task to drive semantic ranking.
	vecs, err := s.EmbedClient.Embed(ctx, []string{in.Task})
	if err != nil {
		if errors.Is(err, embed.ErrUnreachable) {
			return nil, OverviewOutput{Status: "embedding-service-unreachable", Project: p.Root,
				Hint: "embedding service offline — overview requires embeddings; fall back to ask or grep."}, nil
		}
		return nil, OverviewOutput{Status: "error", Hint: fmt.Sprintf("embed: %v", err)}, nil
	}
	vec := vecs[0]

	st, err := store.OpenWith(ctx, p.DBPath, s.StoreOpts)
	if err != nil {
		return nil, OverviewOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
	}
	defer func() { _ = st.Close() }()

	// Semantic search: pull more hits than k so we can aggregate by file.
	hits, err := st.Search(ctx, vec, in.Task, 50)
	if err != nil {
		return nil, OverviewOutput{Status: "error", Hint: fmt.Sprintf("search: %v", err)}, nil
	}

	// Aggregate: max score per file path.
	fileScore := make(map[string]float64, len(hits))
	for _, h := range hits {
		if h.Path == "" {
			continue
		}
		if s := float64(h.Score); s > fileScore[h.Path] {
			fileScore[h.Path] = s
		}
	}

	// Line counts for all indexed code files.
	lineCounts, err := st.CodeFilePaths(ctx)
	if err != nil {
		return nil, OverviewOutput{Status: "error", Hint: fmt.Sprintf("file index: %v", err)}, nil
	}

	// Per-file centrality (best-effort; skip if no graph).
	centrality, _ := st.FileCentrality(ctx)

	// Compute fused score: semantic × (1 + log1p(centrality)).
	type ranked struct {
		path  string
		score float64
	}
	scored := make([]ranked, 0, len(fileScore))
	for path, sem := range fileScore {
		c := centrality[path]
		scored = append(scored, ranked{path, sem * (1 + math.Log1p(c))})
	}
	sort.Slice(scored, func(i, j int) bool {
		if scored[i].score != scored[j].score {
			return scored[i].score > scored[j].score
		}
		return scored[i].path < scored[j].path
	})

	// Build context set.
	contextSet := make(map[string]bool, k)
	context := make([]OverviewFile, 0, k)
	for _, r := range scored {
		if len(context) >= k {
			break
		}
		lc := lineCounts[r.path]
		context = append(context, OverviewFile{
			Path:          r.path,
			Lines:         lc,
			Score:         math.Round(r.score*1000) / 1000,
			SuggestedMode: suggestMode(lc),
		})
		contextSet[r.path] = true
	}

	// Distant: all indexed files not in context, sorted alphabetically.
	distant := make([]string, 0, len(lineCounts))
	for path := range lineCounts {
		if !contextSet[path] {
			distant = append(distant, path)
		}
	}
	sort.Strings(distant)

	const maxDistant = 100
	truncated := distant
	if len(truncated) > maxDistant {
		truncated = truncated[:maxDistant]
	}

	return nil, OverviewOutput{
		Status:       "ok",
		Project:      p.Root,
		Task:         in.Task,
		Context:      context,
		DistantCount: len(distant),
		Distant:      truncated,
	}, nil
}

// suggestMode returns the recommended view_summarize mode for a file of
// the given line count: short files can be read in full; large files are
// best approached via their symbol map.
func suggestMode(lines int) string {
	switch {
	case lines < 200:
		return "full"
	case lines < 800:
		return "signatures"
	default:
		return "map"
	}
}
