package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/alehatsman/dex/internal/chat"
	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/store"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type KnowledgeInput struct {
	ProjectRoot string  `json:"project_root,omitempty"  jsonschema:"absolute path to the project root; defaults to the server's working directory"`
	Action      string  `json:"action"                  jsonschema:"add | list | delete | review | pin | unpin | export | import | consolidate | gc | relate | relations"`
	Archetype   string  `json:"archetype,omitempty"     jsonschema:"Architecture | Gotcha | Convention | Decision | Observation (default) | ReviewFinding (a code-review finding, scope it to the reviewed file so the next toucher sees it — #87) | Hypothesis | Inference | VerifiedFact"`
	Body        string  `json:"body,omitempty"          jsonschema:"fact text for add action; JSON array of {archetype,body,confidence} for import action"`
	Confidence  float64 `json:"confidence,omitempty"    jsonschema:"float 0.0–1.0: how confident this fact is (e.g. 0.9 = high, 0.5 = uncertain). Default 0.8. Strings like 'high' are not valid — pass a number."`
	ID          int64   `json:"id,omitempty"            jsonschema:"fact id for delete action; for relations: the fact whose edges to list"`
	K           int     `json:"k,omitempty"             jsonschema:"max facts to return for list (default 10)"`
	Query       string  `json:"query,omitempty"         jsonschema:"for list: a task/question to recall the most relevant facts for (semantic). Empty = top facts by salience."`
	Scope       string  `json:"scope,omitempty"         jsonschema:"for add: bind this fact to a file glob / path / package (e.g. 'internal/mcp/*_test.go' or 'internal/store') so file verbs (locate/review/read) surface it proactively when they touch a matching path (#645). For list: a path to filter by — returns only notes whose scope binds it, i.e. what would surface on touching it (#653). Empty = unscoped / unfiltered."`
	// SupersedesID is the id of the fact this new note replaces (#606). Setting
	// this calls KnowledgeSupersede: the new note is added and the old one is
	// marked inactive immediately (not waiting for decay). Use action=add with
	// supersedes_id instead of action=supersede for a one-step upsert.
	SupersedesID int64 `json:"supersedes_id,omitempty" jsonschema:"for add: id of the existing fact this new note replaces — marks the old fact inactive immediately (#606)"`
	// ValidUntil is an RFC3339 or date-only (2006-01-02) expiry for time-bounded
	// facts (#618). After this date the fact is excluded from recall. Zero / empty
	// means no expiry. Example: \"2026-12-01\".
	ValidUntil string `json:"valid_until,omitempty"   jsonschema:"for add: RFC3339 or YYYY-MM-DD expiry after which the fact is excluded from recall (e.g. 2026-12-01). Empty = no expiry (#618)."`
	// Evidence marks facts derived from code inspection (#618). Evidence facts
	// decay at half the normal archetype rate.
	Evidence bool `json:"evidence,omitempty"      jsonschema:"for add: true if this fact was derived from direct code inspection (halves decay rate, #618)"`
	// GC tuning — only used for action=gc.
	GCDecayPerDay  float64 `json:"gc_decay_per_day,omitempty"   jsonschema:"for gc: confidence decay per day applied to stale facts (default 0.01)"`
	GCFloor        float64 `json:"gc_floor,omitempty"           jsonschema:"for gc: confidence floor below which a fact is evicted (default 0.05)"`
	GCJaccardMerge float64 `json:"gc_jaccard_merge,omitempty"   jsonschema:"for gc: Jaccard similarity threshold for dedup-merging bodies (default 0.85)"`
	GCMaxFacts     int     `json:"gc_max_facts,omitempty"       jsonschema:"for gc: cap on total active facts; oldest low-confidence facts evicted when exceeded (default 500)"`
	// Relate fields for action=relate (#621).
	RelateFrom int64  `json:"relate_from,omitempty"   jsonschema:"for relate: source fact id"`
	RelateTo   int64  `json:"relate_to,omitempty"     jsonschema:"for relate: target fact id"`
	RelateKind string `json:"relate_kind,omitempty"   jsonschema:"for relate: DependsOn|RelatedTo|Supports|Contradicts|Supersedes"`
	// Diagram requests a Mermaid graph for action=relations when ID=0 (#621).
	Diagram bool `json:"diagram,omitempty"       jsonschema:"for relations: return a Mermaid diagram of all edges instead of a list"`
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
	// Scope is the file glob/path/package this note is bound to (#645); set on
	// scope-filtered list (#653), empty for an unscoped note.
	Scope string `json:"scope,omitempty"`
	// Evidence indicates a code-inspection–derived fact (slower decay, #618).
	Evidence bool `json:"evidence,omitempty"`
	// ValidUntil is the expiry date (RFC3339) for time-bounded facts (#618).
	// Empty when no expiry is set.
	ValidUntil string `json:"valid_until,omitempty"`
}

// KnowledgeRelationOutput is one edge in the relation graph (#621).
type KnowledgeRelationOutput struct {
	FromID    int64   `json:"from_id"`
	ToID      int64   `json:"to_id"`
	Kind      string  `json:"kind"`
	Strength  float64 `json:"strength"`
	Count     int     `json:"count"`
	CreatedAt string  `json:"created_at"`
}

type KnowledgeOutput struct {
	Status string                `json:"status"` // "ok" | "no-index" | "error"
	Hint   string                `json:"hint,omitempty"`
	JSON   string                `json:"json,omitempty"` // export payload (action=export only)
	Facts  []KnowledgeFactOutput `json:"facts"`
	// Similar lists existing notes that overlap the just-added body (action=add
	// only, #606). A non-empty list is a warning, not an error: the agent can
	// `delete` a superseded note or ignore the overlap.
	Similar []KnowledgeFactOutput `json:"similar,omitempty"`
	// ScopeSuggestion is a project file/glob the just-added (unscoped) note seems
	// to be about (#658) — re-add with scope=<this> so file verbs surface it on
	// touch (gotcha-on-touch). Empty when the note named no real path.
	ScopeSuggestion string `json:"scope_suggestion,omitempty"`
	// Review carries advisory cleanup proposals (action=review only, #633). The
	// agent reads them and decides — dex never auto-applies these.
	Review *store.KnowledgeReviewResult `json:"review,omitempty"`
	// Relations is the edge list for action=relate/relations (#621).
	Relations []KnowledgeRelationOutput `json:"relations,omitempty"`
	// Diagram is the Mermaid source for action=relations with diagram=true (#621).
	Diagram string `json:"diagram,omitempty"`
}

// knowledgeSimilarThreshold is the Jaccard word-overlap at which an existing
// note is surfaced as a near-duplicate on add. Deliberately below the GC merge
// threshold (0.85) so the author is warned BEFORE two notes would auto-merge,
// while still requiring substantial overlap to avoid noise.
const knowledgeSimilarThreshold = 0.5

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
		return s.knowledgeAdd(ctx, st, p, in)
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
	case "review":
		return s.knowledgeReview(ctx, st)
	case "pin", "unpin":
		return s.knowledgePin(ctx, st, in)
	case "consolidate":
		return s.knowledgeConsolidate(ctx, st)
	case "gc":
		cfg := store.KnowledgeGCConfig{
			DecayPerDay:     in.GCDecayPerDay,
			ConfidenceFloor: in.GCFloor,
			JaccardMerge:    in.GCJaccardMerge,
			MaxFacts:        in.GCMaxFacts,
		}
		res, err := st.KnowledgeGC(ctx, cfg)
		if err != nil {
			return nil, KnowledgeOutput{Status: "error", Hint: err.Error()}, nil
		}
		return nil, KnowledgeOutput{Status: "ok", Hint: fmt.Sprintf(
			"gc: decayed %d, merged %d, evicted %d, %d facts remain.",
			res.Decayed, res.Merged, res.Evicted, res.Remaining)}, nil
	case "relate":
		return s.knowledgeRelate(ctx, st, in)
	case "relations":
		return s.knowledgeRelations(ctx, st, in)
	case "list", "":
		// fall through to read
	default:
		return nil, KnowledgeOutput{Status: "error", Hint: fmt.Sprintf("unknown action %q — want: add | list | delete | review | pin | unpin | export | import | consolidate | gc | relate | relations", in.Action)}, nil
	}

	// Direct ID lookup: return the exact fact without recall scoring (#764).
	if in.ID > 0 {
		f, err := st.KnowledgeByID(ctx, in.ID)
		if err != nil {
			return nil, KnowledgeOutput{Status: "error", Hint: err.Error()}, nil
		}
		fo := knowledgeFactOut(f)
		if in.Archetype != "" && fo.Archetype != in.Archetype {
			return nil, KnowledgeOutput{Status: "ok", Facts: []KnowledgeFactOutput{}}, nil
		}
		return nil, KnowledgeOutput{Status: "ok", Facts: []KnowledgeFactOutput{fo}}, nil
	}

	// Scope-filtered list (#653): notes whose scope binds the given path — what
	// would proactively surface on touching it — without opening a file. Takes
	// precedence over the semantic Query lane when set.
	if in.Scope != "" {
		facts, err := st.KnowledgeByScope(ctx, in.Scope, in.K)
		if err != nil {
			return nil, KnowledgeOutput{Status: "error", Hint: err.Error()}, nil
		}
		out := KnowledgeOutput{Status: "ok", Facts: []KnowledgeFactOutput{}}
		for _, f := range facts {
			if in.Archetype == "" || f.Archetype == in.Archetype {
				out.Facts = append(out.Facts, knowledgeFactOut(f))
			}
		}
		return nil, out, nil
	}

	var kHint string
	if in.K > 50 {
		kHint = "k capped at 50 — the maximum"
	}
	facts, err := s.recallFacts(ctx, st, in.Query, in.K, false, in.Archetype)
	if err != nil {
		return nil, KnowledgeOutput{Status: "error", Hint: err.Error()}, nil
	}
	out := KnowledgeOutput{Status: "ok", Facts: []KnowledgeFactOutput{}, Hint: kHint}
	for _, f := range facts {
		out.Facts = append(out.Facts, knowledgeFactOut(f))
	}
	return nil, out, nil
}

// scopeTokenRe matches runs of path/glob characters in note text.
var scopeTokenRe = regexp.MustCompile(`[A-Za-z0-9_./*?\-]+`)

// suggestScope returns the first project file path or glob named in body that's
// worth binding a note to (#658), or "" when the note names no real path. A
// token must contain a '/' (a path) or a glob char ('*'/'?'); a bare word like
// "server" or even "foo.go" is too ambiguous to suggest. A glob is returned
// as-is (an explicit scope intent); a plain path is returned only when it
// resolves to a real file or directory under root, so incidental mentions of
// external or nonexistent paths never produce a suggestion.
func suggestScope(root, body string) string {
	seen := map[string]bool{}
	for _, raw := range scopeTokenRe.FindAllString(body, -1) {
		tok := strings.Trim(raw, "./-")
		if tok == "" || seen[tok] {
			continue
		}
		seen[tok] = true
		hasGlob := strings.ContainsAny(tok, "*?")
		if !strings.Contains(tok, "/") && !hasGlob {
			continue // too ambiguous
		}
		if hasGlob {
			// A '/' anchors the glob to a directory; a '.' indicates a file-extension
			// pattern (*.go, *_test.go). A bare *Name with neither is a Go pointer
			// receiver type, not a valid path glob (#766).
			if strings.Contains(tok, "/") || strings.Contains(tok, ".") {
				return tok
			}
			continue
		}
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(tok))); err == nil {
			return tok
		}
	}
	return ""
}

// knowledgeFactOut projects a stored fact into the wire shape, shared by the
// list path and the add near-duplicate warning (#606).
func knowledgeFactOut(f store.KnowledgeFact) KnowledgeFactOutput {
	out := KnowledgeFactOutput{
		ID:            f.ID,
		Archetype:     f.Archetype,
		Body:          f.Body,
		Confidence:    f.Confidence,
		HitCount:      f.HitCount,
		RevisionCount: f.RevisionCount,
		Salience:      f.Salience,
		UpdatedAt:     f.UpdatedAt.Format("2006-01-02 15:04:05"),
		Scope:         f.Scope,
		Evidence:      f.Evidence,
	}
	if !f.ValidUntil.IsZero() {
		out.ValidUntil = f.ValidUntil.Format("2006-01-02")
	}
	return out
}

// parseDate parses a ValidUntil string as RFC3339 or YYYY-MM-DD (UTC midnight).
func parseDate(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	t, err := time.ParseInLocation("2006-01-02", s, time.UTC)
	if err != nil {
		return time.Time{}, fmt.Errorf("expected RFC3339 or YYYY-MM-DD, got %q", s)
	}
	return t, nil
}

// knowledgeAdd handles action=add (#606 #618).
func (s *Server) knowledgeAdd(ctx context.Context, st *store.Store, p *proj.Project, in KnowledgeInput) (*sdk.CallToolResult, KnowledgeOutput, error) {
	if in.Body == "" {
		return nil, KnowledgeOutput{Status: "error", Hint: "body is empty"}, nil
	}
	arch := in.Archetype
	if arch == "" {
		arch = "Observation"
	} else if !validArchetype(arch) {
		return nil, KnowledgeOutput{Status: "error", Hint: fmt.Sprintf(
			"invalid archetype %q — want one of: Architecture, Gotcha, Decision, Convention, Dependency, Pattern, Fact, ReviewFinding, Observation, Hypothesis, Inference, VerifiedFact",
			arch)}, nil
	}
	opts := store.KnowledgeAddOpts{Scope: in.Scope, Evidence: in.Evidence}
	if in.ValidUntil != "" {
		t, err := parseDate(in.ValidUntil)
		if err != nil {
			return nil, KnowledgeOutput{Status: "error", Hint: fmt.Sprintf("valid_until: %v", err)}, nil
		}
		opts.ValidUntil = t
	}
	similar, _ := st.KnowledgeSimilar(ctx, in.Body, knowledgeSimilarThreshold, 3)
	var (
		rev int
		err error
	)
	if in.SupersedesID > 0 {
		rev, err = st.KnowledgeSupersede(ctx, in.SupersedesID, arch, in.Body, in.Confidence, opts)
	} else {
		rev, err = st.KnowledgeAddFull(ctx, arch, in.Body, in.Confidence, opts)
	}
	if err != nil {
		return nil, KnowledgeOutput{Status: "error", Hint: err.Error()}, nil
	}
	s.embedFact(ctx, st, in.Body)
	s.maybeEvict(ctx, st)
	s.activityKnowledgeRecorded(p.Root)
	hint := addHint(in.SupersedesID, rev)
	out := KnowledgeOutput{Status: "ok", Hint: hint}
	for _, sf := range similar {
		out.Similar = append(out.Similar, knowledgeFactOut(sf.KnowledgeFact))
	}
	if len(out.Similar) > 0 {
		out.Hint += fmt.Sprintf(" ⚠ %d similar note(s) already exist (ids %s) — use supersedes_id to replace one.",
			len(out.Similar), similarIDs(out.Similar))
	}
	if in.Scope == "" {
		if sug := suggestScope(p.Root, in.Body); sug != "" {
			out.ScopeSuggestion = sug
			out.Hint += fmt.Sprintf(" 💡 re-add with scope=%q so this surfaces when a verb touches it (gotcha-on-touch).", sug)
		}
	}
	return nil, out, nil
}

func addHint(supersedesID int64, rev int) string {
	if supersedesID > 0 {
		return fmt.Sprintf("Remembered. Note #%d marked inactive (superseded).", supersedesID)
	}
	if rev == 1 {
		return "Confirmed (revision 2)."
	}
	if rev > 1 {
		return fmt.Sprintf("Confirmed (revision %d, confirmed %d×).", rev+1, rev)
	}
	return "Remembered."
}

// knowledgeRelate handles action=relate.
func (s *Server) knowledgeRelate(ctx context.Context, st *store.Store, in KnowledgeInput) (*sdk.CallToolResult, KnowledgeOutput, error) {
	if in.RelateFrom <= 0 || in.RelateTo <= 0 {
		return nil, KnowledgeOutput{Status: "error", Hint: "relate_from and relate_to are required"}, nil
	}
	if in.RelateKind == "" {
		return nil, KnowledgeOutput{Status: "error", Hint: "relate_kind is required: DependsOn|RelatedTo|Supports|Contradicts|Supersedes"}, nil
	}
	if err := st.KnowledgeRelate(ctx, in.RelateFrom, in.RelateTo, in.RelateKind); err != nil {
		return nil, KnowledgeOutput{Status: "error", Hint: err.Error()}, nil
	}
	return nil, KnowledgeOutput{
		Status: "ok",
		Hint:   fmt.Sprintf("Edge #%d -[%s]→ #%d recorded.", in.RelateFrom, in.RelateKind, in.RelateTo),
	}, nil
}

// knowledgeRelations handles action=relations.
func (s *Server) knowledgeRelations(ctx context.Context, st *store.Store, in KnowledgeInput) (*sdk.CallToolResult, KnowledgeOutput, error) {
	if in.Diagram {
		mermaid, err := st.KnowledgeRelationDiagram(ctx, 0.0)
		if err != nil {
			return nil, KnowledgeOutput{Status: "error", Hint: err.Error()}, nil
		}
		return nil, KnowledgeOutput{Status: "ok", Diagram: mermaid}, nil
	}
	if in.ID <= 0 {
		return nil, KnowledgeOutput{Status: "error", Hint: "id is required for relations (or set diagram=true for full graph)"}, nil
	}
	rels, err := st.KnowledgeRelations(ctx, in.ID)
	if err != nil {
		return nil, KnowledgeOutput{Status: "error", Hint: err.Error()}, nil
	}
	out := KnowledgeOutput{Status: "ok"}
	for _, r := range rels {
		out.Relations = append(out.Relations, KnowledgeRelationOutput{
			FromID:    r.FromID,
			ToID:      r.ToID,
			Kind:      string(r.Kind),
			Strength:  r.Strength,
			Count:     r.Count,
			CreatedAt: r.CreatedAt.Format("2006-01-02 15:04:05"),
		})
	}
	if len(out.Relations) == 0 {
		out.Hint = fmt.Sprintf("Fact #%d has no relations yet.", in.ID)
	}
	return nil, out, nil
}

// similarIDs formats the ids of near-duplicate notes for the add hint.
func similarIDs(facts []KnowledgeFactOutput) string {
	ids := make([]string, len(facts))
	for i, f := range facts {
		ids[i] = "#" + strconv.FormatInt(f.ID, 10)
	}
	return strings.Join(ids, ", ")
}

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

// validArchetype reports whether the supplied archetype string names one of the
// known knowledge-fact archetypes (#762). Comparison is case-sensitive because
// the store and the CLI enforce the canonical capitalisation.
func validArchetype(s string) bool {
	switch s {
	case "Architecture", "Gotcha", "Decision", "Convention", "Dependency",
		"Pattern", "Fact", "ReviewFinding", "Observation", "Hypothesis", "Inference", "VerifiedFact":
		return true
	}
	return false
}
