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
	Action      string  `json:"action"                 jsonschema:"add | list | delete | export | import | consolidate | gc"`
	Archetype   string  `json:"archetype,omitempty"    jsonschema:"Architecture | Gotcha | Convention | Decision | Observation (default)"`
	Body        string  `json:"body,omitempty"         jsonschema:"fact text for add action; JSON array of {archetype,body,confidence} for import action"`
	Confidence  float64 `json:"confidence,omitempty"   jsonschema:"float 0.0–1.0: how confident this fact is (e.g. 0.9 = high, 0.5 = uncertain). Default 0.8. Strings like 'high' are not valid — pass a number."`
	ID          int64   `json:"id,omitempty"           jsonschema:"fact id for delete action"`
	K           int     `json:"k,omitempty"            jsonschema:"max facts to return for list (default 10)"`
	Query       string  `json:"query,omitempty"        jsonschema:"for list: a task/question to recall the most relevant facts for (semantic). Empty = top facts by salience."`
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

	st, err := s.openStore(p.DBPath)
	if err != nil {
		return nil, KnowledgeOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
	}

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
		s.embedFact(ctx, st, in.Body)
		s.maybeEvict(ctx, st)
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
	case "gc":
		res, err := st.KnowledgeGC(ctx, store.KnowledgeGCConfig{})
		if err != nil {
			return nil, KnowledgeOutput{Status: "error", Hint: err.Error()}, nil
		}
		return nil, KnowledgeOutput{Status: "ok", Hint: fmt.Sprintf(
			"gc: decayed %d, merged %d, evicted %d, %d facts remain.",
			res.Decayed, res.Merged, res.Evicted, res.Remaining)}, nil
	case "list", "":
		// fall through to read
	default:
		return nil, KnowledgeOutput{Status: "error", Hint: fmt.Sprintf("unknown action %q — want: add | list | delete | export | import | consolidate | gc", in.Action)}, nil
	}

	facts, err := s.recallFacts(ctx, st, in.Query, in.K, false)
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
		s.embedFact(ctx, st, r.Body)
		imported++
	}
	return nil, KnowledgeOutput{Status: "ok", Hint: fmt.Sprintf("imported %d facts", imported)}, nil
}

// knowledgeCap is the soft ceiling on stored facts. Crossing it on add
// triggers an opportunistic GC pass to keep the store (and injected context)
// from growing without bound.
const knowledgeCap = 1000

// maybeEvict runs a lifecycle pass when the fact count exceeds the cap. Cheap
// and best-effort: a no-op below the cap, errors ignored (eviction is
// maintenance, never on the critical add path's success).
func (s *Server) maybeEvict(ctx context.Context, st *store.Store) {
	n, err := st.KnowledgeCount(ctx)
	if err != nil || n <= knowledgeCap {
		return
	}
	_, _ = st.KnowledgeGC(ctx, store.KnowledgeGCConfig{})
}

// embedFact embeds a fact body and stores its vector for semantic recall.
// Best-effort: a nil embed client or an embed error leaves the fact without a
// vector — recall backfills it lazily on a later query.
func (s *Server) embedFact(ctx context.Context, st *store.Store, body string) {
	if s.EmbedClient == nil || strings.TrimSpace(body) == "" {
		return
	}
	vecs, err := s.EmbedClient.Embed(ctx, []string{body})
	if err != nil || len(vecs) == 0 {
		return
	}
	_ = st.KnowledgeUpsertVecByBody(ctx, body, vecs[0])
}

// backfillFactVecs embeds (in one batch) up to 128 facts that lack a stored
// vector, so a store that predates semantic recall — or had facts added while
// the embed endpoint was offline — becomes searchable over a few queries.
func (s *Server) backfillFactVecs(ctx context.Context, st *store.Store) {
	if s.EmbedClient == nil {
		return
	}
	missing, err := st.KnowledgeFactsMissingVec(ctx, 128)
	if err != nil || len(missing) == 0 {
		return
	}
	bodies := make([]string, len(missing))
	for i, f := range missing {
		bodies[i] = f.Body
	}
	vecs, err := s.EmbedClient.Embed(ctx, bodies)
	if err != nil || len(vecs) != len(missing) {
		return
	}
	for i, f := range missing {
		_ = st.KnowledgeUpsertVec(ctx, f.ID, vecs[i])
	}
}

// recallFacts returns up to k facts relevant to query. With a non-empty query
// and a live embed client it ranks by hybrid semantic+quality+recency score
// (backfilling any missing fact vectors first); otherwise it falls back to
// top-salience facts. When bump is true the hit_count of each returned fact is
// incremented (used by ask/overview injection, not by plain `list`).
func (s *Server) recallFacts(ctx context.Context, st *store.Store, query string, k int, bump bool) ([]store.KnowledgeFact, error) {
	var facts []store.KnowledgeFact
	var err error
	if strings.TrimSpace(query) != "" && s.EmbedClient != nil {
		s.backfillFactVecs(ctx, st)
		if vecs, eerr := s.EmbedClient.Embed(ctx, []string{query}); eerr == nil && len(vecs) > 0 {
			facts, err = st.KnowledgeQueryVec(ctx, vecs[0], k)
		}
	}
	if facts == nil && err == nil {
		facts, err = st.KnowledgeQuery(ctx, k)
	}
	if err != nil {
		return nil, err
	}
	if bump {
		for _, f := range facts {
			_ = st.KnowledgeBump(ctx, f.ID)
		}
	}
	return facts, nil
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
