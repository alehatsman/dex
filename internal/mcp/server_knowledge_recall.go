package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/alehatsman/dex/internal/chat"
	"github.com/alehatsman/dex/internal/store"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

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
// top-salience facts unless skipFallback is true. When bump is true the
// hit_count of each returned fact is incremented (used by ask/overview
// injection, not by plain `list`).
//
// archetype, when non-empty, restricts the fallback path to facts of that
// archetype via a SQL WHERE clause (#804), and post-filters the semantic path.
//
// Set skipFallback=true for symbol-oriented callers (locate, review): they
// should show no notes rather than irrelevant top-salience ones when the
// semantic search returns nothing.
func (s *Server) recallFacts(ctx context.Context, st *store.Store, query string, k int, bump bool, archetype string, skipFallback ...bool) ([]store.KnowledgeFact, error) {
	noFallback := len(skipFallback) > 0 && skipFallback[0]
	var facts []store.KnowledgeFact
	var err error
	if strings.TrimSpace(query) != "" && s.EmbedClient != nil {
		s.backfillFactVecs(ctx, st)
		if vecs, eerr := s.EmbedClient.Embed(ctx, []string{query}); eerr == nil && len(vecs) > 0 {
			// skipFallback callers (locate/review) want no note rather than an
			// irrelevant one from a sparse knowledge base (#706).
			// 0.5 empirically separates same-topic from same-codebase-but-unrelated
			// pairs with qwen3-embedding:4b; 0.25 was too permissive (code-aware
			// models score same-codebase concepts ~0.3-0.4 regardless of relevance).
			const minSimSkip = 0.5
			minSim := 0.0
			if noFallback {
				minSim = minSimSkip
			}
			facts, err = st.KnowledgeQueryVec(ctx, vecs[0], k, minSim)
			// Post-filter semantic results by archetype when requested — the vec
			// index has no archetype column, so filtering happens here (#804).
			if archetype != "" && len(facts) > 0 {
				filtered := facts[:0]
				for _, f := range facts {
					if f.Archetype == archetype {
						filtered = append(filtered, f)
					}
				}
				facts = filtered
			}
		}
	}
	if facts == nil && err == nil && !noFallback {
		// Push archetype filter to SQL so top-k by salience doesn't crowd out
		// minority archetypes before the caller can filter (#804).
		if archetype != "" {
			facts, err = st.KnowledgeQueryArchetype(ctx, archetype, k)
		} else {
			facts, err = st.KnowledgeQuery(ctx, k)
		}
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
