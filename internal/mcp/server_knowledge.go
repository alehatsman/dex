package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/alehatsman/dex/internal/store"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type KnowledgeInput struct {
	ProjectRoot string  `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
	Action      string  `json:"action"                 jsonschema:"add | list | delete"`
	Archetype   string  `json:"archetype,omitempty"    jsonschema:"Architecture | Gotcha | Convention | Decision | Observation (default)"`
	Body        string  `json:"body,omitempty"         jsonschema:"fact text for add action"`
	Confidence  float64 `json:"confidence,omitempty"   jsonschema:"0–1, default 0.8"`
	ID          int64   `json:"id,omitempty"           jsonschema:"fact id for delete action"`
	K           int     `json:"k,omitempty"            jsonschema:"max facts to return for list (default 10)"`
}

type KnowledgeFactOutput struct {
	ID         int64   `json:"id"`
	Archetype  string  `json:"archetype"`
	Body       string  `json:"body"`
	Confidence float64 `json:"confidence"`
	HitCount   int     `json:"hit_count"`
	Salience   float64 `json:"salience"`
	UpdatedAt  string  `json:"updated_at"`
}

type KnowledgeOutput struct {
	Status string                `json:"status"` // "ok" | "no-index" | "error"
	Hint   string                `json:"hint,omitempty"`
	Facts  []KnowledgeFactOutput `json:"facts,omitempty"`
}

func (s *Server) knowledge(ctx context.Context, _ *sdk.CallToolRequest, in KnowledgeInput) (*sdk.CallToolResult, KnowledgeOutput, error) {
	p, hint := s.resolveProject(in.ProjectRoot)
	if hint != "" {
		return nil, KnowledgeOutput{Status: "error", Hint: hint}, nil
	}
	if _, err := os.Stat(p.DBPath); errors.Is(err, os.ErrNotExist) {
		return nil, KnowledgeOutput{
			Status: "no-index",
			Hint:   fmt.Sprintf("no index for %s — run `dex index %s` first.", p.Root, p.Root),
		}, nil
	}

	st, err := store.OpenWith(ctx, p.DBPath, s.StoreOpts)
	if err != nil {
		return nil, KnowledgeOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
	}
	defer func() { _ = st.Close() }()

	switch in.Action {
	case "add":
		if in.Body == "" {
			return nil, KnowledgeOutput{Status: "error", Hint: "body is empty"}, nil
		}
		arch := in.Archetype
		if arch == "" {
			arch = "Observation"
		}
		if err := st.KnowledgeAdd(ctx, arch, in.Body, in.Confidence); err != nil {
			return nil, KnowledgeOutput{Status: "error", Hint: err.Error()}, nil
		}
	case "delete":
		if in.ID <= 0 {
			return nil, KnowledgeOutput{Status: "error", Hint: "id is required for delete"}, nil
		}
		if err := st.KnowledgeDelete(ctx, in.ID); err != nil {
			return nil, KnowledgeOutput{Status: "error", Hint: err.Error()}, nil
		}
		return nil, KnowledgeOutput{Status: "ok"}, nil
	case "list", "":
		// fall through to read
	default:
		return nil, KnowledgeOutput{Status: "error", Hint: fmt.Sprintf("unknown action %q — want: add | list | delete", in.Action)}, nil
	}

	facts, err := st.KnowledgeQuery(ctx, in.K)
	if err != nil {
		return nil, KnowledgeOutput{Status: "error", Hint: err.Error()}, nil
	}
	out := KnowledgeOutput{Status: "ok"}
	for _, f := range facts {
		out.Facts = append(out.Facts, KnowledgeFactOutput{
			ID:         f.ID,
			Archetype:  f.Archetype,
			Body:       f.Body,
			Confidence: f.Confidence,
			HitCount:   f.HitCount,
			Salience:   f.Salience,
			UpdatedAt:  f.UpdatedAt.Format("2006-01-02 15:04:05"),
		})
	}
	return nil, out, nil
}
