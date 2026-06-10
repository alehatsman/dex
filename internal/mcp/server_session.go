package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alehatsman/dex/internal/compress"
	dexctx "github.com/alehatsman/dex/internal/ctx"
	"github.com/alehatsman/dex/internal/heatmap"
	"github.com/alehatsman/dex/internal/store"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type SessionInput struct {
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
	Action      string `json:"action"                 jsonschema:"set_task | add_note | add_file | get | clear | snapshot | budget | heatmap"`
	Task        string `json:"task,omitempty"         jsonschema:"task description for set_task action"`
	Note        string `json:"note,omitempty"         jsonschema:"note text for add_note action"`
	File        string `json:"file,omitempty"         jsonschema:"relative file path for add_file action"`
	Op          string `json:"op,omitempty"           jsonschema:"file operation: read (default) or write"`
	WindowSize  int    `json:"window_size,omitempty"  jsonschema:"context window size in tokens for budget action (default 128000)"`
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
	// Budget fields (action=budget only)
	WindowSize      int     `json:"window_size,omitempty"`
	UsedTokens      int     `json:"used_tokens,omitempty"`
	RemainingTokens int     `json:"remaining_tokens,omitempty"`
	Utilization     float64 `json:"utilization,omitempty"`
	Recommendation  string  `json:"recommendation,omitempty"`
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
	case "budget":
		return s.sessionBudget(ctx, st, p.Root, in.WindowSize)
	case "heatmap":
		return s.sessionHeatmap(p.CacheDir)
	case "get", "":
		// fall through to the read below
	default:
		return nil, SessionOutput{Status: "error", Hint: fmt.Sprintf("unknown action %q — want: set_task | add_note | add_file | get | clear | snapshot | budget | heatmap", in.Action)}, nil
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

// sessionBudget estimates context window utilization for the current session.
// It reads file sizes from disk to approximate tokens consumed, then returns
// utilization, remaining capacity, and a pressure-level recommendation.
func (s *Server) sessionBudget(ctx context.Context, st *store.Store, projectRoot string, windowSize int) (*sdk.CallToolResult, SessionOutput, error) {
	if windowSize <= 0 {
		windowSize = dexctx.DefaultWindowSize
	}

	ss, ok, err := st.SessionGet(ctx)
	if err != nil {
		return nil, SessionOutput{Status: "error", Hint: err.Error()}, nil
	}
	if !ok {
		ledger := dexctx.Ledger{WindowSize: windowSize}
		return nil, SessionOutput{
			Status:          "ok",
			Hint:            "no active session",
			WindowSize:      ledger.WindowSize,
			UsedTokens:      0,
			RemainingTokens: ledger.Remaining(),
			Utilization:     0,
			Recommendation:  string(ledger.Pressure()),
		}, nil
	}

	var used int
	// task + notes: rough token estimate from byte length
	used += dexctx.BytesToTokens(int64(len(ss.Task) + len(ss.Notes)))

	// files: estimate from actual file size on disk (deduped by path)
	seen := make(map[string]struct{}, len(ss.Files))
	for _, f := range ss.Files {
		if _, dup := seen[f.Path]; dup {
			continue
		}
		seen[f.Path] = struct{}{}
		abs := filepath.Join(projectRoot, f.Path)
		if info, statErr := os.Stat(abs); statErr == nil {
			used += dexctx.BytesToTokens(info.Size())
		}
	}

	ledger := dexctx.Ledger{WindowSize: windowSize, UsedTokens: used}
	return nil, SessionOutput{
		Status:          "ok",
		WindowSize:      ledger.WindowSize,
		UsedTokens:      ledger.UsedTokens,
		RemainingTokens: ledger.Remaining(),
		Utilization:     ledger.Utilization(),
		Recommendation:  string(ledger.Pressure()),
	}, nil
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

// sessionHeatmap returns the file access heatmap for a project.
func (s *Server) sessionHeatmap(cacheDir string) (*sdk.CallToolResult, SessionOutput, error) {
	hm := heatmap.Load(cacheDir)
	return nil, SessionOutput{Status: "ok", Content: hm.Format(15)}, nil
}

// FeedbackInput is the input for the ctx_feedback MCP tool.
type FeedbackInput struct {
	ProjectRoot     string  `json:"project_root,omitempty"      jsonschema:"absolute path to the project root; defaults to the server's working directory"`
	Intent          string  `json:"intent"                      jsonschema:"current task intent: read | search | refactor | generate | test | debug | review"`
	OutputRatio     float64 `json:"output_ratio"                jsonschema:"LLM output tokens divided by context tokens for this turn (0.0–1.0+); values below 0.05 indicate the context was unhelpful"`
	CtxReadLastMode string  `json:"ctx_read_last_mode,omitempty" jsonschema:"the file_view mode used on the last file read this turn (full|signatures|map|aggressive); omit if no file_view was called"`
}

// FeedbackOutput is the result of ctx_feedback.
type FeedbackOutput struct {
	Status string `json:"status"` // "ok" | "error"
	Hint   string `json:"hint,omitempty"`
}

// feedback handles the ctx_feedback MCP tool. It records an output-ratio
// observation into the per-project adaptive policy so future file_view
// mode selections can avoid modes that consistently produce thin output.
// Feedback is the public HTTP-callable wrapper for the ctx_feedback tool.
func (s *Server) Feedback(ctx context.Context, in FeedbackInput) (FeedbackOutput, error) {
	_, out, err := s.feedback(ctx, nil, in)
	return out, err
}

func (s *Server) feedback(_ context.Context, _ *sdk.CallToolRequest, in FeedbackInput) (*sdk.CallToolResult, FeedbackOutput, error) {
	p, hint := s.resolveProject(in.ProjectRoot)
	if hint != "" {
		return nil, FeedbackOutput{Status: "error", Hint: hint}, nil
	}
	if in.Intent == "" {
		return nil, FeedbackOutput{Status: "error", Hint: "intent is required"}, nil
	}
	if in.CtxReadLastMode == "" {
		// No file_view was called this turn — nothing to record.
		return nil, FeedbackOutput{Status: "ok"}, nil
	}
	pt := compress.LoadPolicy(p.CacheDir)
	pt.RecordFeedback(in.Intent, in.CtxReadLastMode, in.OutputRatio)
	return nil, FeedbackOutput{Status: "ok"}, nil
}
