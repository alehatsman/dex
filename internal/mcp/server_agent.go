package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/alehatsman/dex/internal/store"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// AgentInput is the request schema for the agent coordination bus tool.
type AgentInput struct {
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
	Action      string `json:"action"                 jsonschema:"announce | post | read | list | diary | recall_diary | diaries"`
	AgentID     string `json:"agent_id,omitempty"     jsonschema:"agent identifier (required for announce, post, diary, recall_diary)"`
	Role        string `json:"role,omitempty"         jsonschema:"agent role description (used with announce)"`
	Topic       string `json:"topic,omitempty"        jsonschema:"message topic/channel — groups related findings (used with post and read)"`
	Category    string `json:"category,omitempty"     jsonschema:"semantic kind label — e.g. finding | plan | error | note (used with post and read); entry type for diary: discovery | decision | blocker | progress | insight"`
	Query       string `json:"query,omitempty"        jsonschema:"full-text search query over message bodies (used with read; powered by FTS5)"`
	Body        string `json:"body,omitempty"         jsonschema:"message body (required for post)"`
	Content     string `json:"content,omitempty"      jsonschema:"diary entry content (required for diary)"`
	SinceID     int64  `json:"since_id,omitempty"     jsonschema:"return messages with id > since_id for incremental polling (used with read)"`
	Limit       int    `json:"limit,omitempty"        jsonschema:"max messages to return (default 50, max 200; used with read)"`
}

// AgentEntry is one registered agent in the bus.
type AgentEntry struct {
	ID          string `json:"id"`
	Role        string `json:"role,omitempty"`
	AnnouncedAt string `json:"announced_at"`
	LastSeenAt  string `json:"last_seen_at"`
}

// AgentMessageEntry is one message on the bus.
type AgentMessageEntry struct {
	ID       int64  `json:"id"`
	AgentID  string `json:"agent_id"`
	Role     string `json:"role,omitempty"`
	Topic    string `json:"topic,omitempty"`
	Category string `json:"category,omitempty"`
	Body     string `json:"body"`
	PostedAt string `json:"posted_at"`
}

// AgentOutput is the response for the agent tool.
type AgentOutput struct {
	Status       string              `json:"status"` // "ok" | "no-index" | "error"
	Hint         string              `json:"hint,omitempty"`
	Agents       []AgentEntry        `json:"agents,omitempty"`
	Messages     []AgentMessageEntry `json:"messages,omitempty"`
	MessageID    int64               `json:"message_id,omitempty"` // populated by post
	DiaryEntries []DiaryEntry        `json:"diary_entries,omitempty"`
	DiaryAgents  []DiaryListEntry    `json:"diary_agents,omitempty"`
}

func (s *Server) agent(ctx context.Context, _ *sdk.CallToolRequest, in AgentInput) (*sdk.CallToolResult, AgentOutput, error) {
	// Diary actions are agent-scoped (not project-scoped) — handle before DB open.
	switch in.Action {
	case "diary":
		if in.AgentID == "" {
			return nil, AgentOutput{Status: "error", Hint: "agent_id is required for diary"}, nil
		}
		if in.Content == "" {
			return nil, AgentOutput{Status: "error", Hint: "content is required for diary"}, nil
		}
		cat := in.Category
		if cat == "" {
			cat = "insight"
		}
		entry, err := DiaryAppend(s.IndexDir, in.AgentID, cat, in.Content)
		if err != nil {
			return nil, AgentOutput{Status: "error", Hint: fmt.Sprintf("diary write: %v", err)}, nil
		}
		return nil, AgentOutput{Status: "ok", DiaryEntries: []DiaryEntry{entry}}, nil

	case "recall_diary":
		if in.AgentID == "" {
			return nil, AgentOutput{Status: "error", Hint: "agent_id is required for recall_diary"}, nil
		}
		n := in.Limit
		if n <= 0 {
			n = diaryRecallN
		}
		entries, err := DiaryRecall(s.IndexDir, in.AgentID, n)
		if err != nil {
			return nil, AgentOutput{Status: "error", Hint: fmt.Sprintf("diary read: %v", err)}, nil
		}
		return nil, AgentOutput{Status: "ok", DiaryEntries: entries}, nil

	case "diaries":
		list, err := DiaryList(s.IndexDir)
		if err != nil {
			return nil, AgentOutput{Status: "error", Hint: fmt.Sprintf("diary list: %v", err)}, nil
		}
		return nil, AgentOutput{Status: "ok", DiaryAgents: list}, nil
	}

	// Bus actions require a project-scoped index DB.
	p, hint := s.resolveProject(in.ProjectRoot)
	if hint != "" {
		return nil, AgentOutput{Status: "error", Hint: hint}, nil
	}
	if _, err := os.Stat(p.DBPath); errors.Is(err, os.ErrNotExist) {
		return nil, AgentOutput{
			Status: "no-index",
			Hint:   fmt.Sprintf("no index for %s — run `dex index %s` first.", p.Root, p.Root),
		}, nil
	}

	st, err := store.OpenWith(ctx, p.DBPath, s.StoreOpts)
	if err != nil {
		return nil, AgentOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
	}
	defer func() { _ = st.Close() }()

	switch in.Action {
	case "announce":
		if in.AgentID == "" {
			return nil, AgentOutput{Status: "error", Hint: "agent_id is required for announce"}, nil
		}
		if err := st.AgentAnnounce(ctx, in.AgentID, in.Role); err != nil {
			return nil, AgentOutput{Status: "error", Hint: err.Error()}, nil
		}
		return nil, AgentOutput{Status: "ok"}, nil

	case "post":
		if in.AgentID == "" {
			return nil, AgentOutput{Status: "error", Hint: "agent_id is required for post"}, nil
		}
		if in.Body == "" {
			return nil, AgentOutput{Status: "error", Hint: "body is required for post"}, nil
		}
		msgID, err := st.AgentPost(ctx, in.AgentID, in.Topic, in.Category, in.Body)
		if err != nil {
			return nil, AgentOutput{Status: "error", Hint: err.Error()}, nil
		}
		return nil, AgentOutput{Status: "ok", MessageID: msgID}, nil

	case "read":
		msgs, err := st.AgentRead(ctx, in.Topic, in.Category, in.Query, in.SinceID, in.Limit)
		if err != nil {
			return nil, AgentOutput{Status: "error", Hint: err.Error()}, nil
		}
		out := AgentOutput{Status: "ok"}
		for _, m := range msgs {
			out.Messages = append(out.Messages, AgentMessageEntry{
				ID:       m.ID,
				AgentID:  m.AgentID,
				Role:     m.Role,
				Topic:    m.Topic,
				Category: m.Category,
				Body:     m.Body,
				PostedAt: m.PostedAt.Format("2006-01-02 15:04:05"),
			})
		}
		return nil, out, nil

	case "list":
		agents, err := st.AgentList(ctx)
		if err != nil {
			return nil, AgentOutput{Status: "error", Hint: err.Error()}, nil
		}
		out := AgentOutput{Status: "ok"}
		for _, a := range agents {
			out.Agents = append(out.Agents, AgentEntry{
				ID:          a.ID,
				Role:        a.Role,
				AnnouncedAt: a.AnnouncedAt.Format("2006-01-02 15:04:05"),
				LastSeenAt:  a.LastSeenAt.Format("2006-01-02 15:04:05"),
			})
		}
		return nil, out, nil

	default:
		return nil, AgentOutput{
			Status: "error",
			Hint:   fmt.Sprintf("unknown action %q — want: announce | post | read | list | diary | recall_diary | diaries", in.Action),
		}, nil
	}
}
