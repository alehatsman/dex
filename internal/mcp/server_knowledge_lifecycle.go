package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/alehatsman/dex/internal/store"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// knowledgeExportRow is the portable JSON shape used for export/import.
// Includes scope and ranking signals so a round-trip preserves gotcha-on-touch
// bindings and salience ordering (#645, #678).
type knowledgeExportRow struct {
	Archetype     string  `json:"archetype"`
	Body          string  `json:"body"`
	Confidence    float64 `json:"confidence"`
	Scope         string  `json:"scope,omitempty"`
	HitCount      int     `json:"hit_count,omitempty"`
	RevisionCount int     `json:"revision_count,omitempty"`
}

func (s *Server) knowledgeExport(ctx context.Context, st *store.Store) (*sdk.CallToolResult, KnowledgeOutput, error) {
	facts, err := st.KnowledgeExportAll(ctx) // uncapped — a 50-cap would silently drop notes (#647)
	if err != nil {
		return nil, KnowledgeOutput{Status: "error", Hint: err.Error()}, nil
	}
	rows := make([]knowledgeExportRow, len(facts))
	for i, f := range facts {
		rows[i] = knowledgeExportRow{
			Archetype:     f.Archetype,
			Body:          f.Body,
			Confidence:    f.Confidence,
			Scope:         f.Scope,
			HitCount:      f.HitCount,
			RevisionCount: f.RevisionCount,
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
		// MCP content-envelope: the body param may arrive as a JSON-stringified
		// string wrapping the actual array (the same double-encode pattern seen in
		// #734 / parseAskResponse). Try to unwrap one string layer before failing.
		var inner string
		if json.Unmarshal([]byte(body), &inner) == nil {
			if err2 := json.Unmarshal([]byte(inner), &rows); err2 != nil {
				return nil, KnowledgeOutput{Status: "error", Hint: fmt.Sprintf("parse JSON: %v — expected [{archetype,body,confidence},...]", err2)}, nil
			}
		} else {
			return nil, KnowledgeOutput{Status: "error", Hint: fmt.Sprintf("parse JSON: %v — expected [{archetype,body,confidence},...]", err)}, nil
		}
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
		if _, err := st.KnowledgeAddScoped(ctx, arch, r.Body, conf, r.Scope); err != nil {
			return nil, KnowledgeOutput{Status: "error", Hint: fmt.Sprintf("import fact %q: %v", r.Body[:min(40, len(r.Body))], err)}, nil
		}
		s.embedFact(ctx, st, r.Body)
		imported++
	}
	return nil, KnowledgeOutput{Status: "ok", Hint: fmt.Sprintf("imported %d facts", imported)}, nil
}

// knowledgeReview returns advisory cleanup proposals (action=review, #633).
// Read-only — dex never auto-applies them.
func (s *Server) knowledgeReview(ctx context.Context, st *store.Store) (*sdk.CallToolResult, KnowledgeOutput, error) {
	res, err := st.KnowledgeReview(ctx)
	if err != nil {
		return nil, KnowledgeOutput{Status: "error", Hint: err.Error()}, nil
	}
	hint := "Knowledge store is tidy — nothing to review."
	if res.Total > 0 {
		hint = fmt.Sprintf("%d proposal(s): %d merge, %d overlap, %d stale. Read-only — apply with delete/add/pin.",
			res.Total, len(res.Merge), len(res.Overlap), len(res.Stale))
	}
	return nil, KnowledgeOutput{Status: "ok", Hint: hint, Review: &res}, nil
}

// knowledgePin sets or clears a fact's pinned flag (action=pin|unpin, #633).
func (s *Server) knowledgePin(ctx context.Context, st *store.Store, in KnowledgeInput) (*sdk.CallToolResult, KnowledgeOutput, error) {
	if in.ID <= 0 {
		return nil, KnowledgeOutput{Status: "error", Hint: "id is required for pin/unpin"}, nil
	}
	if err := st.KnowledgeSetPinned(ctx, in.ID, in.Action == "pin"); err != nil {
		return nil, KnowledgeOutput{Status: "error", Hint: err.Error()}, nil
	}
	return nil, KnowledgeOutput{Status: "ok", Hint: fmt.Sprintf("%sned fact #%d.", in.Action, in.ID)}, nil
}

// defaultNotesReviewThreshold is the proposal count at/above which session-start
// orientation nudges to run `dex notes review` (#633).
const defaultNotesReviewThreshold = 5

// notesReviewThreshold reads DEX_NOTES_REVIEW_THRESHOLD (values < 1 fall back to
// the default).
func notesReviewThreshold() int {
	if v := os.Getenv("DEX_NOTES_REVIEW_THRESHOLD"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			return n
		}
	}
	return defaultNotesReviewThreshold
}

// pendingReviewCount returns how many `notes review` proposals the store holds,
// best-effort (0 on any error). It only drives the advisory session-start nudge,
// so it must never block or fail orientation. The store handle is owned by the
// server (openStore is cached) — do not close it here.
func (s *Server) pendingReviewCount(ctx context.Context, dbPath string) int {
	if _, err := os.Stat(dbPath); err != nil {
		return 0
	}
	st, err := s.openStore(dbPath)
	if err != nil {
		return 0
	}
	res, err := st.KnowledgeReview(ctx)
	if err != nil {
		return 0
	}
	return res.Total
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
