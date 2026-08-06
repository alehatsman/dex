package mcp

import (
	"context"
	"fmt"

	"github.com/alehatsman/dex/internal/shadow"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// maxCheckpointDiffBytes caps the unified diff returned by action=diff so a
// large checkpoint comparison never blows an agent's context window. The agent
// can narrow with from/to or read specific files.
const maxCheckpointDiffBytes = 32 * 1024

// CheckpointInput drives the `checkpoint` tool — a per-project shadow git
// history of the working tree, isolated from the user's .git (#608).
type CheckpointInput struct {
	Action      string `json:"action" jsonschema:"snapshot (commit the working tree to the shadow repo) | log (list checkpoints) | diff (unified diff between two checkpoints)"`
	Message     string `json:"message,omitempty" jsonschema:"commit message for action=snapshot (default: a timestamp)"`
	From        string `json:"from,omitempty" jsonschema:"left checkpoint ref for action=diff (default HEAD~1)"`
	To          string `json:"to,omitempty" jsonschema:"right checkpoint ref for action=diff (default HEAD)"`
	Limit       int    `json:"limit,omitempty" jsonschema:"max checkpoints to list for action=log (default 20, max 200)"`
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project or git worktree you are working in. The server cannot see your shell's directory; when working in a worktree different from where the server started, pass that worktree's path"`
}

// CheckpointOutput is the checkpoint tool result. Only the fields relevant to
// the requested action are populated.
type CheckpointOutput struct {
	Status       string          `json:"status"` // "ok" | "error"
	Hint         string          `json:"hint,omitempty"`
	Project      string          `json:"project,omitempty"`
	SHA          string          `json:"sha,omitempty"`           // snapshot: the checkpoint commit
	FilesChanged int             `json:"files_changed,omitempty"` // snapshot
	Created      bool            `json:"created,omitempty"`       // snapshot: false when unchanged
	Commits      []shadow.Commit `json:"commits,omitempty"`       // log
	Diff         string          `json:"diff,omitempty"`          // diff
	Truncated    bool            `json:"truncated,omitempty"`     // diff exceeded the byte cap
}

// Checkpoint is the public (non-SDK) entry point for REST and direct callers.
func (s *Server) Checkpoint(ctx context.Context, in CheckpointInput) (CheckpointOutput, error) {
	_, out, err := s.checkpoint(ctx, nil, in)
	return out, err
}

func (s *Server) checkpoint(ctx context.Context, _ *sdk.CallToolRequest, in CheckpointInput) (*sdk.CallToolResult, CheckpointOutput, error) {
	p, hint := s.resolveProject(ctx, in.ProjectRoot)
	if hint != "" {
		return nil, CheckpointOutput{Status: "error", Hint: hint}, nil
	}
	if err := p.EnsureCacheDir(); err != nil {
		return nil, CheckpointOutput{Status: "error", Hint: fmt.Sprintf("cache dir: %v", err)}, nil
	}
	repo := shadow.Open(p.CacheDir, p.Root)

	switch in.Action {
	case "snapshot":
		res, err := repo.Snapshot(ctx, in.Message)
		if err != nil {
			return nil, CheckpointOutput{Status: "error", Hint: err.Error()}, nil
		}
		out := CheckpointOutput{Status: "ok", Project: p.Root, SHA: res.SHA, FilesChanged: res.FilesChanged, Created: res.Created}
		if res.Created {
			out.Hint = fmt.Sprintf("checkpoint %s — %d file(s) changed.", short(res.SHA), res.FilesChanged)
		} else {
			out.Hint = "no changes since the last checkpoint."
		}
		return nil, out, nil

	case "log":
		commits, err := repo.Log(ctx, in.Limit)
		if err != nil {
			return nil, CheckpointOutput{Status: "error", Hint: err.Error()}, nil
		}
		out := CheckpointOutput{Status: "ok", Project: p.Root, Commits: commits}
		if len(commits) == 0 {
			out.Hint = "no checkpoints yet — run action=snapshot first."
		}
		return nil, out, nil

	case "diff":
		diff, err := repo.Diff(ctx, in.From, in.To)
		if err != nil {
			return nil, CheckpointOutput{Status: "error", Hint: err.Error()}, nil
		}
		out := CheckpointOutput{Status: "ok", Project: p.Root}
		if len(diff) > maxCheckpointDiffBytes {
			diff = diff[:maxCheckpointDiffBytes]
			out.Truncated = true
			out.Hint = "diff truncated — narrow with from/to."
		}
		out.Diff = diff
		if diff == "" && !out.Truncated {
			out.Hint = "no checkpoints to diff yet — run action=snapshot first."
		}
		return nil, out, nil

	default:
		return nil, CheckpointOutput{Status: "error",
			Hint: fmt.Sprintf("unknown action %q — want: snapshot | log | diff", in.Action)}, nil
	}
}

// short trims a SHA to 12 chars for display.
func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
