package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ─── tool: graph_neighbors ────────────────────────────────────────────────

type RelatedInput struct {
	Path        string `json:"path" jsonschema:"relative file path of the source chunk (e.g. 'internal/store/store.go')"`
	StartLine   int    `json:"start_line" jsonschema:"start line of the source chunk (1-indexed)"`
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project or git worktree you are working in. The server cannot see your shell's directory; when working in a worktree different from where the server started, pass that worktree's path"`
	K           int    `json:"k,omitempty" jsonschema:"number of related chunks to return (default 8, max 30)"`
	// Threshold drops hits below this cosine similarity (0..1); 0 keeps all.
	// The `similar` verb sets it to return only genuinely near-duplicate blocks.
	Threshold float32 `json:"threshold,omitempty" jsonschema:"drop hits below this cosine similarity, 0..1 (default 0 = keep all)"`
}

type RelatedOutput struct {
	Status  string      `json:"status"` // "ok" | "no-index" | "not-found" | "embedding-service-unreachable" | "error"
	Hint    string      `json:"hint,omitempty"`
	Project string      `json:"project,omitempty"`
	Hits    []SearchHit `json:"hits"`
}

func (s *Server) related(ctx context.Context, _ *sdk.CallToolRequest, in RelatedInput) (*sdk.CallToolResult, RelatedOutput, error) {
	if strings.TrimSpace(in.Path) == "" {
		return nil, RelatedOutput{Status: "error", Hint: "path is empty"}, nil
	}
	if in.StartLine <= 0 {
		return nil, RelatedOutput{Status: "error", Hint: "start_line must be ≥ 1"}, nil
	}
	p, hint := s.resolveProject(ctx, in.ProjectRoot)
	if hint != "" {
		return nil, RelatedOutput{Status: "error", Hint: hint}, nil
	}
	if _, err := os.Stat(p.DBPath); errors.Is(err, os.ErrNotExist) {
		return nil, RelatedOutput{Status: "no-index", Project: p.Root,
			Hint: fmt.Sprintf("no index for %s — run `dex index %s` first.", p.Root, p.Root)}, nil
	}
	k := in.K
	if k <= 0 {
		k = 8
	}
	if k > 30 {
		k = 30
	}
	st, err := s.openStore(p.DBPath)
	if err != nil {
		return nil, RelatedOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
	}
	hits, err := st.RelatedChunks(ctx, in.Path, in.StartLine, k)
	if err != nil {
		if strings.Contains(err.Error(), "no chunk at") {
			return nil, RelatedOutput{Status: "not-found", Project: p.Root,
				Hint: err.Error() + " — check that path and start_line match an indexed chunk exactly."}, nil
		}
		return nil, RelatedOutput{Status: "error", Hint: fmt.Sprintf("related: %v", err)}, nil
	}
	out := RelatedOutput{Status: "ok", Project: p.Root, Hits: []SearchHit{}}
	for _, h := range hits {
		if in.Threshold > 0 && h.Score < in.Threshold {
			continue // below the similarity floor requested by `similar`
		}
		out.Hits = append(out.Hits, SearchHit{
			Path:      h.Path,
			Kind:      h.Kind,
			StartLine: h.StartLine,
			EndLine:   h.EndLine,
			SortScore: h.DisplayScore(),
			Score:     h.Score,
			Content:   h.Content,
		})
	}
	stampSearchHandles(out.Hits)
	return nil, out, nil
}
