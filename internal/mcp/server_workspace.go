package mcp

// server_workspace.go — search_workspace MCP tool.
//
// Loads .dex/workspace.yml from the project root, runs hybrid search
// independently per listed project, merges results with RRF (k=60),
// and tags each hit with [project:label] so the agent knows which
// project it came from.

import (
	"context"
	"fmt"
	"sort"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/store"
	"github.com/alehatsman/dex/internal/workspace"
)

// WorkspaceSearchInput mirrors SearchInput but is workspace-scoped.
type WorkspaceSearchInput struct {
	Query       string   `json:"query" jsonschema:"natural-language or code query"`
	ProjectRoot string   `json:"project_root,omitempty" jsonschema:"absolute path of the anchor project whose .dex/workspace.yml is read; defaults to server working directory"`
	K           int      `json:"k,omitempty" jsonschema:"total results to return across all projects (default 8, max 30)"`
	Exclude     []string `json:"exclude,omitempty" jsonschema:"path prefixes to skip (e.g. ['vendor/', 'internal/legacy/'])"`
	Languages   []string `json:"languages,omitempty" jsonschema:"restrict results to these languages"`
	PathGlob    string   `json:"path_glob,omitempty" jsonschema:"glob pattern matched against relative file path"`
}

// WorkspaceSearchOutput is the response from search_workspace.
type WorkspaceSearchOutput struct {
	Status   string      `json:"status"` // "ok" | "no-workspace" | "no-index" | "embedding-service-unreachable" | "error"
	Hint     string      `json:"hint,omitempty"`
	Project  string      `json:"project,omitempty"`
	Projects int         `json:"projects,omitempty"` // number of projects searched
	Total    int         `json:"total"`
	Hits     []SearchHit `json:"hits"`
}

func (s *Server) WorkspaceSearch(ctx context.Context, in WorkspaceSearchInput) (WorkspaceSearchOutput, error) {
	_, out, err := s.workspaceSearch(ctx, nil, in)
	return out, err
}

// labelledHit pairs a store.Hit with the project label it came from.
type labelledHit struct {
	h     store.Hit
	label string
}

func (s *Server) workspaceSearch(ctx context.Context, _ *sdk.CallToolRequest, in WorkspaceSearchInput) (*sdk.CallToolResult, WorkspaceSearchOutput, error) {
	p, hint := s.resolveProject(in.ProjectRoot)
	if hint != "" {
		return nil, WorkspaceSearchOutput{Status: "error", Hint: hint}, nil
	}

	cfg, err := workspace.Load(p.Root)
	if err != nil {
		return nil, WorkspaceSearchOutput{Status: "error", Project: p.Root,
			Hint: fmt.Sprintf("load workspace.yml: %v", err)}, nil
	}
	if cfg == nil || len(cfg.Projects) == 0 {
		return nil, WorkspaceSearchOutput{
			Status:  "no-workspace",
			Project: p.Root,
			Hint:    "no .dex/workspace.yml found or it lists no projects; create one to enable workspace search",
		}, nil
	}

	k := in.K
	if k <= 0 {
		k = 8
	}
	if k > 30 {
		k = 30
	}
	candidateK := k * 5
	if candidateK < 30 {
		candidateK = 30
	}

	// Embed the query once.
	vecs, err := s.EmbedClient.Embed(ctx, []string{in.Query})
	if err != nil {
		return nil, WorkspaceSearchOutput{
			Status: "embedding-service-unreachable",
			Hint:   "embedding service offline — workspace search requires embeddings",
		}, nil
	}
	queryVec := vecs[0]

	exts := langToExtensions(in.Languages)

	// Search each project and collect hits with project label.
	var allHits []labelledHit

	var searchedProjects int
	for _, pe := range cfg.Projects {
		pp, err := proj.Resolve(pe.Path, s.IndexDir)
		if err != nil {
			continue // project doesn't exist yet — skip silently
		}
		st, err := store.OpenWith(ctx, pp.DBPath, s.StoreOpts)
		if err != nil {
			continue // not indexed — skip
		}
		hits, sErr := st.Search(ctx, queryVec, in.Query, candidateK)
		_ = st.Close()
		if sErr != nil || len(hits) == 0 {
			continue
		}
		searchedProjects++
		hits = filterHits(hits, exts, in.PathGlob, candidateK)
		for _, h := range hits {
			if !excluded(h.Path, in.Exclude) {
				allHits = append(allHits, labelledHit{h: h, label: pe.Label})
			}
		}
	}

	if searchedProjects == 0 {
		return nil, WorkspaceSearchOutput{
			Status:  "no-index",
			Project: p.Root,
			Hint:    "none of the workspace projects are indexed; run `dex index <path>` for each",
		}, nil
	}

	// RRF merge across projects (k=60.0 is the standard lean-ctx constant).
	merged := workspaceRRF(allHits, k)

	out := WorkspaceSearchOutput{
		Status:   "ok",
		Project:  p.Root,
		Projects: searchedProjects,
		Total:    len(merged),
	}
	for _, item := range merged {
		sh := hitToSearchHit(item.h)
		sh.Path = fmt.Sprintf("[project:%s] %s", item.label, sh.Path)
		out.Hits = append(out.Hits, sh)
	}
	return nil, out, nil
}

// workspaceRRF merges labelledHits from multiple projects using Reciprocal Rank
// Fusion (k=60.0). Hits from each project are treated as one ranked list.
// The dedup key is "label|path|start_line" to avoid collapsing same-path
// chunks that differ across projects.
func workspaceRRF(hits []labelledHit, topK int) []labelledHit {
	if len(hits) == 0 {
		return nil
	}
	const rrfK = 60.0
	type key struct {
		label string
		path  string
		start int
	}
	// Group hits by project label to rank within each project.
	byProject := make(map[string][]labelledHit)
	for _, h := range hits {
		byProject[h.label] = append(byProject[h.label], h)
	}
	// Within each project, hits are already ordered by score (descending from Store.Search).
	scores := make(map[key]float32)
	hitFor := make(map[key]labelledHit)
	for _, group := range byProject {
		for rank, h := range group {
			k := key{h.label, h.h.Path, h.h.StartLine}
			scores[k] += 1.0 / (rrfK + float32(rank) + 1)
			if _, seen := hitFor[k]; !seen {
				hitFor[k] = h
			}
		}
	}
	type scored struct {
		k     key
		score float32
	}
	result := make([]scored, 0, len(scores))
	for k, s := range scores {
		result = append(result, scored{k, s})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].score > result[j].score })
	if len(result) > topK {
		result = result[:topK]
	}
	out := make([]labelledHit, 0, len(result))
	for _, r := range result {
		out = append(out, hitFor[r.k])
	}
	return out
}

// hitToSearchHit converts a store.Hit to the MCP SearchHit format.
// Reuses the inline conversion logic from the search handler to avoid drift.
func hitToSearchHit(h store.Hit) SearchHit {
	return SearchHit{
		Path:        h.Path,
		Kind:        h.Kind,
		StartLine:   h.StartLine,
		EndLine:     h.EndLine,
		Score:       h.Score,
		BM25Score:   h.BM25Score,
		RRFScore:    h.RRFScore,
		RerankScore: h.RerankScore,
	}
}
