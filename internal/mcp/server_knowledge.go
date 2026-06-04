package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/alehatsman/dex/internal/chat"
	"github.com/alehatsman/dex/internal/store"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type KnowledgeInput struct {
	ProjectRoot string  `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
	Action      string  `json:"action"                 jsonschema:"add | list | delete | export | import | consolidate"`
	Archetype   string  `json:"archetype,omitempty"    jsonschema:"Architecture | Gotcha | Convention | Decision | Observation (default)"`
	Body        string  `json:"body,omitempty"         jsonschema:"fact text for add action; JSON array of {archetype,body,confidence} for import action"`
	Confidence  float64 `json:"confidence,omitempty"   jsonschema:"0–1, default 0.8"`
	ID          int64   `json:"id,omitempty"           jsonschema:"fact id for delete action"`
	K           int     `json:"k,omitempty"            jsonschema:"max facts to return for list (default 10)"`
}

type KnowledgeFactOutput struct {
	ID            int64   `json:"id"`
	Archetype     string  `json:"archetype"`
	Body          string  `json:"body"`
	Confidence    float64 `json:"confidence"`
	HitCount      int     `json:"hit_count"`
	RevisionCount int     `json:"revision_count,omitempty"`
	Salience      float64 `json:"salience"`
	UpdatedAt     string  `json:"updated_at"`
}

type KnowledgeOutput struct {
	Status string                `json:"status"` // "ok" | "no-index" | "error"
	Hint   string                `json:"hint,omitempty"`
	JSON   string                `json:"json,omitempty"` // export payload (action=export only)
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
		rev, err := st.KnowledgeAdd(ctx, arch, in.Body, in.Confidence)
		if err != nil {
			return nil, KnowledgeOutput{Status: "error", Hint: err.Error()}, nil
		}
		s.activityKnowledgeRecorded(p.Root)
		hint := "Remembered."
		if rev == 1 {
			hint = "Confirmed (revision 2)."
		} else if rev > 1 {
			hint = fmt.Sprintf("Confirmed (revision %d, confirmed %d×).", rev+1, rev)
		}
		return nil, KnowledgeOutput{Status: "ok", Hint: hint}, nil
	case "delete":
		if in.ID <= 0 {
			return nil, KnowledgeOutput{Status: "error", Hint: "id is required for delete"}, nil
		}
		if err := st.KnowledgeDelete(ctx, in.ID); err != nil {
			return nil, KnowledgeOutput{Status: "error", Hint: err.Error()}, nil
		}
		return nil, KnowledgeOutput{Status: "ok"}, nil
	case "export":
		return s.knowledgeExport(ctx, st)
	case "import":
		if in.Body == "" {
			return nil, KnowledgeOutput{Status: "error", Hint: "body must be a JSON array [{archetype,body,confidence},...] for import"}, nil
		}
		return s.knowledgeImport(ctx, st, in.Body)
	case "consolidate":
		return s.knowledgeConsolidate(ctx, st)
	case "list", "":
		// fall through to read
	default:
		return nil, KnowledgeOutput{Status: "error", Hint: fmt.Sprintf("unknown action %q — want: add | list | delete | export | import | consolidate", in.Action)}, nil
	}

	facts, err := st.KnowledgeQuery(ctx, in.K)
	if err != nil {
		return nil, KnowledgeOutput{Status: "error", Hint: err.Error()}, nil
	}
	out := KnowledgeOutput{Status: "ok"}
	for _, f := range facts {
		out.Facts = append(out.Facts, KnowledgeFactOutput{
			ID:            f.ID,
			Archetype:     f.Archetype,
			Body:          f.Body,
			Confidence:    f.Confidence,
			HitCount:      f.HitCount,
			RevisionCount: f.RevisionCount,
			Salience:      f.Salience,
			UpdatedAt:     f.UpdatedAt.Format("2006-01-02 15:04:05"),
		})
	}
	return nil, out, nil
}

// knowledgeExportRow is the portable JSON shape used for export/import.
type knowledgeExportRow struct {
	Archetype  string  `json:"archetype"`
	Body       string  `json:"body"`
	Confidence float64 `json:"confidence"`
}

func (s *Server) knowledgeExport(ctx context.Context, st *store.Store) (*sdk.CallToolResult, KnowledgeOutput, error) {
	facts, err := st.KnowledgeQuery(ctx, 50)
	if err != nil {
		return nil, KnowledgeOutput{Status: "error", Hint: err.Error()}, nil
	}
	rows := make([]knowledgeExportRow, len(facts))
	for i, f := range facts {
		rows[i] = knowledgeExportRow{
			Archetype:  f.Archetype,
			Body:       f.Body,
			Confidence: f.Confidence,
		}
	}
	data, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		return nil, KnowledgeOutput{Status: "error", Hint: err.Error()}, nil
	}
	return nil, KnowledgeOutput{Status: "ok", JSON: string(data)}, nil
}

func (s *Server) knowledgeImport(ctx context.Context, st *store.Store, body string) (*sdk.CallToolResult, KnowledgeOutput, error) {
	var rows []knowledgeExportRow
	if err := json.Unmarshal([]byte(body), &rows); err != nil {
		return nil, KnowledgeOutput{Status: "error", Hint: fmt.Sprintf("parse JSON: %v — expected [{archetype,body,confidence},...]", err)}, nil
	}
	imported := 0
	for _, r := range rows {
		if r.Body == "" {
			continue
		}
		arch := r.Archetype
		if arch == "" {
			arch = "Observation"
		}
		conf := r.Confidence
		if conf <= 0 {
			conf = 0.7 // slightly below default 0.8 for imported facts
		}
		if _, err := st.KnowledgeAdd(ctx, arch, r.Body, conf); err != nil {
			return nil, KnowledgeOutput{Status: "error", Hint: fmt.Sprintf("import fact %q: %v", r.Body[:min(40, len(r.Body))], err)}, nil
		}
		imported++
	}
	return nil, KnowledgeOutput{Status: "ok", Hint: fmt.Sprintf("imported %d facts", imported)}, nil
}

func (s *Server) knowledgeConsolidate(ctx context.Context, st *store.Store) (*sdk.CallToolResult, KnowledgeOutput, error) {
	if s.ChatClient == nil {
		return nil, KnowledgeOutput{Status: "error", Hint: "consolidate requires a chat model (set DEX_CHAT_URL)"}, nil
	}
	ss, ok, err := st.SessionGet(ctx)
	if err != nil {
		return nil, KnowledgeOutput{Status: "error", Hint: err.Error()}, nil
	}
	if !ok || (ss.Task == "" && ss.Notes == "") {
		return nil, KnowledgeOutput{Status: "ok", Hint: "no session content to consolidate"}, nil
	}
	var parts []string
	if ss.Task != "" {
		parts = append(parts, "Task: "+ss.Task)
	}
	if ss.Notes != "" {
		parts = append(parts, "Session notes:\n"+ss.Notes)
	}
	system := `Extract 3-7 reusable project knowledge facts from the session content below. ` +
		`Output ONLY a JSON array: [{"archetype":"Architecture|Gotcha|Convention|Decision|Observation|Dependency|Pattern|Fact","body":"one concise sentence","confidence":0.7}]. ` +
		`Only include facts worth remembering across sessions. No preamble, no explanation.`
	resp, err := s.ChatClient.Generate(ctx, []chat.Message{
		{Role: "system", Content: system},
		{Role: "user", Content: strings.Join(parts, "\n\n")},
	}, chat.Options{MaxTokens: 1024})
	if err != nil {
		if errors.Is(err, chat.ErrUnreachable) {
			return nil, KnowledgeOutput{Status: "chat-service-unreachable", Hint: "chat service is offline"}, nil
		}
		return nil, KnowledgeOutput{Status: "error", Hint: fmt.Sprintf("chat: %v", err)}, nil
	}
	raw := extractJSONArray(resp.Content)
	var rows []knowledgeExportRow
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		return nil, KnowledgeOutput{Status: "error", Hint: fmt.Sprintf("parse LLM response: %v — got: %s", err, truncate(raw, 200))}, nil
	}
	stored := 0
	for _, r := range rows {
		if r.Body == "" {
			continue
		}
		arch := r.Archetype
		if arch == "" {
			arch = "Observation"
		}
		conf := r.Confidence
		if conf <= 0 {
			conf = 0.75
		}
		if _, err := st.KnowledgeAdd(ctx, arch, r.Body, conf); err != nil {
			continue
		}
		stored++
	}
	// Return the stored facts.
	facts, _ := st.KnowledgeQuery(ctx, stored+5)
	out := KnowledgeOutput{Status: "ok", Hint: fmt.Sprintf("consolidated %d facts from session", stored)}
	for _, f := range facts {
		out.Facts = append(out.Facts, KnowledgeFactOutput{
			ID:            f.ID,
			Archetype:     f.Archetype,
			Body:          f.Body,
			Confidence:    f.Confidence,
			HitCount:      f.HitCount,
			RevisionCount: f.RevisionCount,
			Salience:      f.Salience,
			UpdatedAt:     f.UpdatedAt.Format("2006-01-02 15:04:05"),
		})
	}
	return nil, out, nil
}

// extractJSONArray pulls a JSON array out of an LLM response that might
// wrap it in markdown code fences.
func extractJSONArray(s string) string {
	// Strip ```json ... ``` or ``` ... ``` fences.
	for _, fence := range []string{"```json", "```"} {
		if i := strings.Index(s, fence); i >= 0 {
			s = s[i+len(fence):]
			if j := strings.Index(s, "```"); j >= 0 {
				return strings.TrimSpace(s[:j])
			}
		}
	}
	// Fall back: find first '['.
	if i := strings.Index(s, "["); i >= 0 {
		return strings.TrimSpace(s[i:])
	}
	return strings.TrimSpace(s)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
