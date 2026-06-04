package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/alehatsman/dex/internal/store"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type SessionInput struct {
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
	Action      string `json:"action"                 jsonschema:"set_task | add_note | add_file | get | clear | snapshot"`
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
	Content   string              `json:"content,omitempty"` // snapshot markdown (action=snapshot only)
	ID        int64               `json:"id,omitempty"`
	StartedAt string              `json:"started_at,omitempty"`
	UpdatedAt string              `json:"updated_at,omitempty"`
	Duration  string              `json:"duration,omitempty"` // human-readable session age
	Task      string              `json:"task,omitempty"`
	Notes     string              `json:"notes,omitempty"`
	NoteCount int                 `json:"note_count,omitempty"`
	FileCount int                 `json:"file_count,omitempty"`
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
	case "snapshot":
		return s.sessionSnapshot(ctx, st, p.Root)
	case "get", "":
		// fall through to the read below
	default:
		return nil, SessionOutput{Status: "error", Hint: fmt.Sprintf("unknown action %q — want: set_task | add_note | add_file | get | clear | snapshot", in.Action)}, nil
	}

	ss, ok, err := st.SessionGet(ctx)
	if err != nil {
		return nil, SessionOutput{Status: "error", Hint: err.Error()}, nil
	}
	if !ok {
		return nil, SessionOutput{Status: "ok", Hint: "no session started — use action=set_task to begin"}, nil
	}

	noteCount := 0
	if ss.Notes != "" {
		noteCount = strings.Count(ss.Notes, "\n") + 1
	}
	out := SessionOutput{
		Status:    "ok",
		ID:        ss.ID,
		StartedAt: ss.StartedAt.Format("2006-01-02 15:04:05"),
		UpdatedAt: ss.UpdatedAt.Format("2006-01-02 15:04:05"),
		Duration:  formatDuration(time.Since(ss.StartedAt)),
		Task:      ss.Task,
		Notes:     ss.Notes,
		NoteCount: noteCount,
		FileCount: len(ss.Files),
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

// sessionSnapshot generates a structured recovery block for use after context
// compaction. It lists file_view and search_semantic calls that will
// cheaply re-establish context for the declared task and touched files.
func (s *Server) sessionSnapshot(ctx context.Context, st *store.Store, projectRoot string) (*sdk.CallToolResult, SessionOutput, error) {
	ss, ok, err := st.SessionGet(ctx)
	if err != nil {
		return nil, SessionOutput{Status: "error", Hint: err.Error()}, nil
	}
	if !ok {
		return nil, SessionOutput{Status: "ok", Hint: "no active session — start one with action=set_task"}, nil
	}

	var b strings.Builder
	b.WriteString("# Session Recovery Snapshot\n\n")

	if ss.Task != "" {
		b.WriteString("## Task\n")
		b.WriteString(ss.Task)
		b.WriteString("\n\n")
		b.WriteString("## Re-establish context\n\n")
		fmt.Fprintf(&b, "```\nsearch_semantic: {\"query\": %q, \"project_root\": %q}\n```\n\n", ss.Task, projectRoot)
	}

	if len(ss.Files) > 0 {
		b.WriteString("## Files to re-read\n\n")
		seen := make(map[string]struct{}, len(ss.Files))
		for _, f := range ss.Files {
			if _, dup := seen[f.Path]; dup {
				continue
			}
			seen[f.Path] = struct{}{}
			mode := "signatures"
			if f.Op == "write" {
				mode = "map"
			}
			fmt.Fprintf(&b, "```\nfile_view: {\"path\": %q, \"mode\": %q, \"project_root\": %q}\n```\n",
				f.Path, mode, projectRoot)
		}
		b.WriteString("\n")
	}

	if ss.Notes != "" {
		b.WriteString("## Session notes\n\n")
		b.WriteString(ss.Notes)
		b.WriteString("\n")
	}

	return nil, SessionOutput{
		Status:  "ok",
		Content: b.String(),
	}, nil
}

// formatDuration produces a compact human-readable duration string.
func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm%02ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}
