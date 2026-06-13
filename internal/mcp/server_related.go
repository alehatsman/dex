package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/alehatsman/dex/internal/embed"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ─── tool: graph_neighbors ────────────────────────────────────────────────

type RelatedInput struct {
	Path        string `json:"path" jsonschema:"relative file path of the source chunk (e.g. 'internal/store/store.go')"`
	StartLine   int    `json:"start_line" jsonschema:"start line of the source chunk (1-indexed)"`
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
	K           int    `json:"k,omitempty" jsonschema:"number of related chunks to return (default 8, max 30)"`
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
	p, hint := s.resolveProject(in.ProjectRoot)
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
		out.Hits = append(out.Hits, SearchHit{
			Path:      h.Path,
			Kind:      h.Kind,
			StartLine: h.StartLine,
			EndLine:   h.EndLine,
			Score:     h.Score,
			Content:   h.Content,
		})
	}
	stampSearchHandles(out.Hits)
	return nil, out, nil
}

// ─── tool: search_similar ────────────────────────────────────────────────

type FindRelatedInput struct {
	FilePath    string   `json:"file_path" jsonschema:"relative path of the anchor file (e.g. 'internal/mcp/server.go')"`
	Line        int      `json:"line" jsonschema:"line number inside the anchor file (1-indexed); resolves to the containing chunk"`
	ProjectRoot string   `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
	K           int      `json:"k,omitempty" jsonschema:"number of results to return (default 8, max 30)"`
	Exclude     []string `json:"exclude,omitempty" jsonschema:"path prefixes to skip"`
	Languages   []string `json:"languages,omitempty" jsonschema:"restrict results to these languages (e.g. ['go','typescript'])"`
	PathGlob    string   `json:"path_glob,omitempty" jsonschema:"glob pattern matched against relative file path (e.g. 'internal/**')"`
}

type FindRelatedOutput struct {
	Status string `json:"status"` // "ok" | "no-index" | "not-found" | "embedding-service-unreachable" | "error"
	Hint   string `json:"hint,omitempty"`
	// Source is the resolved anchor chunk.
	Source  *SearchHit  `json:"source,omitempty"`
	Project string      `json:"project,omitempty"`
	Hits    []SearchHit `json:"hits"`
}

func (s *Server) findRelated(ctx context.Context, _ *sdk.CallToolRequest, in FindRelatedInput) (*sdk.CallToolResult, FindRelatedOutput, error) {
	if strings.TrimSpace(in.FilePath) == "" {
		return nil, FindRelatedOutput{Status: "error", Hint: "file_path is empty"}, nil
	}
	if in.Line <= 0 {
		return nil, FindRelatedOutput{Status: "error", Hint: "line must be ≥ 1"}, nil
	}
	p, hint := s.resolveProject(in.ProjectRoot)
	if hint != "" {
		return nil, FindRelatedOutput{Status: "error", Hint: hint}, nil
	}
	if _, err := os.Stat(p.DBPath); errors.Is(err, os.ErrNotExist) {
		return nil, FindRelatedOutput{Status: "no-index", Project: p.Root,
			Hint: fmt.Sprintf("no index for %s — run `dex index %s` first.", p.Root, p.Root)}, nil
	}

	k := in.K
	if k <= 0 {
		k = 8
	}
	if k > 30 {
		k = 30
	}
	candidateK := k
	if len(in.Languages) > 0 || in.PathGlob != "" {
		candidateK = k * 10
		if candidateK < 50 {
			candidateK = 50
		}
		if candidateK > 500 {
			candidateK = 500
		}
	}

	st, err := s.openStore(p.DBPath)
	if err != nil {
		return nil, FindRelatedOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
	}

	src, err := st.ChunkAt(ctx, in.FilePath, in.Line)
	if err != nil {
		if strings.Contains(err.Error(), "no chunk at") {
			return nil, FindRelatedOutput{Status: "not-found", Project: p.Root,
				Hint: err.Error() + " — check that file_path and line match an indexed chunk."}, nil
		}
		return nil, FindRelatedOutput{Status: "error", Hint: fmt.Sprintf("chunk lookup: %v", err)}, nil
	}

	em := s.EmbedClient
	vecs, err := em.Embed(ctx, []string{src.Content})
	if err != nil {
		if errors.Is(err, embed.ErrUnreachable) {
			return nil, FindRelatedOutput{Status: "embedding-service-unreachable",
				Hint: "the local embedding service is offline — fall back to grep / Glob for this query."}, nil
		}
		return nil, FindRelatedOutput{Status: "error", Hint: fmt.Sprintf("embed: %v", err)}, nil
	}

	hits, err := st.SearchFused(ctx, vecs[0], src.Content, candidateK)
	if err != nil {
		return nil, FindRelatedOutput{Status: "error", Hint: fmt.Sprintf("search: %v", err)}, nil
	}

	// Exclude the source chunk itself.
	filtered := hits[:0]
	for _, h := range hits {
		if h.Path == src.Path && h.StartLine == src.StartLine {
			continue
		}
		filtered = append(filtered, h)
	}
	hits = filtered

	// Symbol leg.
	idents := extractIdentifiers(src.Content)
	if len(idents) > 0 {
		symPool := candidateK * 3
		if symPool < 15 {
			symPool = 15
		}
		if symHits := collectSymbolHits(ctx, st, idents, symPool); len(symHits) > 0 {
			hits = fuseWithSymbols(hits, symHits, candidateK)
			// Re-exclude source after symbol fusion.
			out2 := hits[:0]
			for _, h := range hits {
				if h.Path == src.Path && h.StartLine == src.StartLine {
					continue
				}
				out2 = append(out2, h)
			}
			hits = out2
		}
	}

	// Graph-proximity lane: spreading activation from session-recent files and
	// the current semantic hits. Silently skips when no session exists or the
	// graph hasn't been built — never fails the search.
	hits = st.FuseSpreadingActivation(ctx, hits, vecs[0], candidateK)

	hits, err = st.RerankFused(ctx, src.Content, hits, candidateK)
	if err != nil {
		return nil, FindRelatedOutput{Status: "error", Hint: fmt.Sprintf("rerank: %v", err)}, nil
	}

	var sessionTask string
	if ss, ok, err2 := st.SessionGet(ctx); err2 == nil && ok {
		sessionTask = ss.Task
	}
	hits = ecsRerank(hits, extractTaskKWs(sessionTask))

	exts := langToExtensions(in.Languages)
	hits = filterHits(hits, exts, in.PathGlob, k)

	out := FindRelatedOutput{
		Status:  "ok",
		Project: p.Root,
		Hits:    []SearchHit{},
		Source: &SearchHit{
			Path:      src.Path,
			Kind:      src.Kind,
			StartLine: src.StartLine,
			EndLine:   src.EndLine,
			Content:   src.Content,
		},
	}
	for _, h := range hits {
		if excluded(h.Path, in.Exclude) {
			continue
		}
		out.Hits = append(out.Hits, SearchHit{
			Path:        h.Path,
			Kind:        h.Kind,
			StartLine:   h.StartLine,
			EndLine:     h.EndLine,
			Score:       h.Score,
			BM25Score:   h.BM25Score,
			RRFScore:    h.RRFScore,
			RerankScore: h.RerankScore,
			Role:        formatRole(h.Name, h.InDegree, h.OutDegree, h.CrossPkgCallers, h.Betweenness),
			Content:     h.Content,
		})
	}
	stampSearchHandles(out.Hits)
	return nil, out, nil
}
