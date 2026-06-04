package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/alehatsman/dex/internal/store"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type SessionInput struct {
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
	Action      string `json:"action"                 jsonschema:"set_task | add_note | add_file | get | clear"`
	Task        string `json:"task,omitempty"         jsonschema:"task description for set_task action"`
	Note        string `json:"note,omitempty"         jsonschema:"note text for add_note action"`
	File        string `json:"file,omitempty"         jsonschema:"relative file path for add_file action"`
	Op          string `json:"op,omitempty"           jsonschema:"file operation: read (default) or write"`
}

type SessionFileOutput struct {
	Path      string `json:"path"`
	Op        string `json:"op"`
	TouchedAt string `json:"touched_at"`
}

type SessionOutput struct {
	Status    string              `json:"status"` // "ok" | "no-index" | "error"
	Hint      string              `json:"hint,omitempty"`
	ID        int64               `json:"id,omitempty"`
	StartedAt string              `json:"started_at,omitempty"`
	UpdatedAt string              `json:"updated_at,omitempty"`
	Task      string              `json:"task,omitempty"`
	Notes     string              `json:"notes,omitempty"`
	Files     []SessionFileOutput `json:"files,omitempty"`
}

func (s *Server) session(ctx context.Context, _ *sdk.CallToolRequest, in SessionInput) (*sdk.CallToolResult, SessionOutput, error) {
	p, hint := s.resolveProject(in.ProjectRoot)
	if hint != "" {
		return nil, SessionOutput{Status: "error", Hint: hint}, nil
	}
	if _, err := os.Stat(p.DBPath); errors.Is(err, os.ErrNotExist) {
		return nil, SessionOutput{
			Status: "no-index",
			Hint:   fmt.Sprintf("no index for %s — run `dex index %s` first.", p.Root, p.Root),
		}, nil
	}

	st, err := store.OpenWith(ctx, p.DBPath, s.StoreOpts)
	if err != nil {
		return nil, SessionOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
	}
	defer func() { _ = st.Close() }()

	switch in.Action {
	case "set_task":
		if err := st.SessionSetTask(ctx, in.Task); err != nil {
			return nil, SessionOutput{Status: "error", Hint: err.Error()}, nil
		}
	case "add_note":
		if in.Note == "" {
			return nil, SessionOutput{Status: "error", Hint: "note is empty"}, nil
		}
		if err := st.SessionAddNote(ctx, in.Note); err != nil {
			return nil, SessionOutput{Status: "error", Hint: err.Error()}, nil
		}
	case "add_file":
		if in.File == "" {
			return nil, SessionOutput{Status: "error", Hint: "file is empty"}, nil
		}
		op := in.Op
		if op == "" {
			op = "read"
		}
		if err := st.SessionAddFile(ctx, in.File, op); err != nil {
			return nil, SessionOutput{Status: "error", Hint: err.Error()}, nil
		}
	case "clear":
		if err := st.SessionClear(ctx); err != nil {
			return nil, SessionOutput{Status: "error", Hint: err.Error()}, nil
		}
		return nil, SessionOutput{Status: "ok"}, nil
	case "get", "":
		// fall through to the read below
	default:
		return nil, SessionOutput{Status: "error", Hint: fmt.Sprintf("unknown action %q — want: set_task | add_note | add_file | get | clear", in.Action)}, nil
	}

	ss, ok, err := st.SessionGet(ctx)
	if err != nil {
		return nil, SessionOutput{Status: "error", Hint: err.Error()}, nil
	}
	if !ok {
		return nil, SessionOutput{Status: "ok", Hint: "no session started — use action=set_task to begin"}, nil
	}

	out := SessionOutput{
		Status:    "ok",
		ID:        ss.ID,
		StartedAt: ss.StartedAt.Format("2006-01-02 15:04:05"),
		UpdatedAt: ss.UpdatedAt.Format("2006-01-02 15:04:05"),
		Task:      ss.Task,
		Notes:     ss.Notes,
	}
	for _, f := range ss.Files {
		out.Files = append(out.Files, SessionFileOutput{
			Path:      f.Path,
			Op:        f.Op,
			TouchedAt: f.TouchedAt.Format("2006-01-02 15:04:05"),
		})
	}
	return nil, out, nil
}
