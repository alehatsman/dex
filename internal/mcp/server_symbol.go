package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/alehatsman/dex/internal/throttle"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ─── tool: search_symbol ──────────────────────────────────────────────────

type FindSymbolInput struct {
	Name        string `json:"name" jsonschema:"exact identifier name to look up (case-sensitive, e.g. 'MyFunc', 'HTTPHandler')"`
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
	K           int    `json:"k,omitempty" jsonschema:"max results to return (default 10)"`
}

type FindSymbolOutput struct {
	Status  string      `json:"status"` // "ok" | "no-index" | "not-found" | "error"
	Hint    string      `json:"hint,omitempty"`
	Project string      `json:"project,omitempty"`
	Hits    []SearchHit `json:"hits"`
}

func (s *Server) findSymbol(ctx context.Context, _ *sdk.CallToolRequest, in FindSymbolInput) (*sdk.CallToolResult, FindSymbolOutput, error) {
	if strings.TrimSpace(in.Name) == "" {
		return nil, FindSymbolOutput{Status: "error", Hint: "name is empty"}, nil
	}
	p, hint := s.resolveProject(in.ProjectRoot)
	if hint != "" {
		return nil, FindSymbolOutput{Status: "error", Hint: hint}, nil
	}
	if _, err := os.Stat(p.DBPath); errors.Is(err, os.ErrNotExist) {
		return nil, FindSymbolOutput{Status: "no-index", Project: p.Root,
			Hint: fmt.Sprintf("no index for %s — run `dex index %s` first.", p.Root, p.Root)}, nil
	}
	st, err := s.openStore(p.DBPath)
	if err != nil {
		return nil, FindSymbolOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
	}
	ldLevel, ldHint := s.ld().Check("lookup", throttle.ArgsKey(in.Name), true)
	if ldLevel == throttle.Block {
		return nil, FindSymbolOutput{Status: "loop-blocked", Project: p.Root, Hint: ldHint}, nil
	}

	hits, err := st.FindSymbol(ctx, in.Name, in.K)
	if err != nil {
		return nil, FindSymbolOutput{Status: "error", Hint: fmt.Sprintf("lookup: %v", err)}, nil
	}
	out := FindSymbolOutput{Status: "ok", Project: p.Root, Hits: []SearchHit{}}
	if ldHint != "" {
		out.Hint = ldHint
	}
	if len(hits) == 0 {
		out.Status = "not-found"
		hint := fmt.Sprintf("no chunk with name=%q in the index; check spelling or re-index if recently added.", in.Name)
		// Near-miss surface: substring matches give the agent something
		// real to retry with instead of guessing. Errors are non-fatal —
		// the original "not-found" hint is still useful on its own.
		if cands, candErr := st.FindSymbolCandidates(ctx, in.Name, 5); candErr == nil && len(cands) > 0 {
			hint += " Did you mean: " + strings.Join(cands, ", ") + "?"
		}
		out.Hint = hint
		return nil, out, nil
	}
	for _, h := range hits {
		out.Hits = append(out.Hits, SearchHit{
			Path:      h.Path,
			Kind:      h.Kind,
			StartLine: h.StartLine,
			EndLine:   h.EndLine,
			SortScore: 1.0, // exact-match lookup: all hits rank equally
			Score:     1.0,
			Role:      formatRole(h.Name, h.InDegree, h.OutDegree, h.CrossPkgCallers, h.Betweenness),
			Content:   h.Content,
		})
	}
	if ldLevel == throttle.Reduce && len(out.Hits) > 5 {
		out.Hits = out.Hits[:5]
		out.Hint = ldHint + " [reduced: showing top 5]"
	}
	stampSearchHandles(out.Hits)
	return nil, out, nil
}
