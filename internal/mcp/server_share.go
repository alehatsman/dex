package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/alehatsman/dex/internal/store"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ShareInput is the request schema for the ctx_share tool.
type ShareInput struct {
	ProjectRoot string `json:"project_root,omitempty"  jsonschema:"absolute path to the project root; defaults to the server's working directory"`
	Action      string `json:"action"                  jsonschema:"push | pull | list | clear"`
	Path        string `json:"path,omitempty"          jsonschema:"project-relative file path (required for push and pull)"`
	ContentHash string `json:"content_hash,omitempty"  jsonschema:"SHA-256 hex of the raw file content (required for push; used by pull to detect staleness)"`
	Content     string `json:"content,omitempty"       jsonschema:"compressed or processed file content to cache (required for push)"`
	AgentID     string `json:"agent_id,omitempty"      jsonschema:"agent identifier — recorded as the pusher (optional for push)"`
}

// ShareCacheEntry is one entry returned by list.
type ShareCacheEntry struct {
	Path        string `json:"path"`
	ContentHash string `json:"content_hash"`
	PushedBy    string `json:"pushed_by,omitempty"`
	PushedAt    string `json:"pushed_at"`
	HitCount    int    `json:"hit_count"`
}

// ShareOutput is the response for the ctx_share tool.
type ShareOutput struct {
	Status   string            `json:"status"` // "ok" | "stale" | "not-found" | "no-index" | "error"
	Hint     string            `json:"hint,omitempty"`
	Content  string            `json:"content,omitempty"`   // populated by pull on hit
	HitCount int               `json:"hit_count,omitempty"` // populated by pull on hit
	Entries  []ShareCacheEntry `json:"entries,omitempty"`   // populated by list
	Cleared  int64             `json:"cleared,omitempty"`   // populated by clear
}

func (s *Server) share(ctx context.Context, _ *sdk.CallToolRequest, in ShareInput) (*sdk.CallToolResult, ShareOutput, error) {
	p, hint := s.resolveProject(in.ProjectRoot)
	if hint != "" {
		return nil, ShareOutput{Status: "error", Hint: hint}, nil
	}
	if _, err := os.Stat(p.DBPath); errors.Is(err, os.ErrNotExist) {
		return nil, ShareOutput{
			Status: "no-index",
			Hint:   fmt.Sprintf("no index for %s — run `dex index %s` first.", p.Root, p.Root),
		}, nil
	}

	st, err := store.OpenWith(ctx, p.DBPath, s.StoreOpts)
	if err != nil {
		return nil, ShareOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
	}
	defer func() { _ = st.Close() }()

	switch in.Action {
	case "push":
		if in.Path == "" {
			return nil, ShareOutput{Status: "error", Hint: "path is required for push"}, nil
		}
		if in.ContentHash == "" {
			return nil, ShareOutput{Status: "error", Hint: "content_hash is required for push"}, nil
		}
		if in.Content == "" {
			return nil, ShareOutput{Status: "error", Hint: "content is required for push"}, nil
		}
		if err := st.SharePush(ctx, in.Path, in.ContentHash, in.Content, in.AgentID); err != nil {
			return nil, ShareOutput{Status: "error", Hint: err.Error()}, nil
		}
		return nil, ShareOutput{Status: "ok", Hint: fmt.Sprintf("cached %s", in.Path)}, nil

	case "pull":
		if in.Path == "" {
			return nil, ShareOutput{Status: "error", Hint: "path is required for pull"}, nil
		}
		if in.ContentHash == "" {
			return nil, ShareOutput{Status: "error", Hint: "content_hash is required for pull"}, nil
		}
		content, hitCount, ok, err := st.SharePull(ctx, in.Path, in.ContentHash)
		if err != nil {
			return nil, ShareOutput{Status: "error", Hint: err.Error()}, nil
		}
		if !ok {
			return nil, ShareOutput{Status: "stale", Hint: "no fresh cache entry — read the file and push to share with peers"}, nil
		}
		return nil, ShareOutput{Status: "ok", Content: content, HitCount: hitCount}, nil

	case "list":
		entries, err := st.ShareList(ctx)
		if err != nil {
			return nil, ShareOutput{Status: "error", Hint: err.Error()}, nil
		}
		out := ShareOutput{Status: "ok"}
		for _, e := range entries {
			out.Entries = append(out.Entries, ShareCacheEntry{
				Path:        e.Path,
				ContentHash: e.ContentHash,
				PushedBy:    e.PushedBy,
				PushedAt:    e.PushedAt.Format(time.DateTime),
				HitCount:    e.HitCount,
			})
		}
		return nil, out, nil

	case "clear":
		n, err := st.ShareClear(ctx)
		if err != nil {
			return nil, ShareOutput{Status: "error", Hint: err.Error()}, nil
		}
		return nil, ShareOutput{Status: "ok", Cleared: n, Hint: fmt.Sprintf("evicted %d entries", n)}, nil

	default:
		return nil, ShareOutput{
			Status: "error",
			Hint:   fmt.Sprintf("unknown action %q — want: push | pull | list | clear", in.Action),
		}, nil
	}
}
