package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/alehatsman/dex/internal/store"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type KnowledgeInput struct {
	ProjectRoot string  `json:"project_root,omitempty"  jsonschema:"absolute path to the project or git worktree you are working in. The server cannot see your shell's directory; when working in a worktree different from where the server started, pass that worktree's path"`
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
	// NeedsVerification is set at recall when every code referent the fact names
	// (path, path:line, or symbol) has gone dead against the current index (#167).
	// Computed, never persisted; a flag, not a downgrade — confidence is untouched.
	NeedsVerification bool `json:"needs_verification,omitempty"`
	// VerificationNote names the dead referents behind NeedsVerification so the
	// agent knows exactly what to re-check. Empty unless NeedsVerification is set.
	VerificationNote string `json:"verification_note,omitempty"`
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

// knowledgeReferentMax caps how many referent-overlap supersede candidates a
// write surfaces (#167 Part 3) — advisory, kept small so the nudge stays legible.
const knowledgeReferentMax = 3

func (s *Server) knowledge(ctx context.Context, _ *sdk.CallToolRequest, in KnowledgeInput) (*sdk.CallToolResult, KnowledgeOutput, error) {
	p, hint := s.resolveProject(ctx, in.ProjectRoot)
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
		s.annotateLiveness(ctx, st, out.Facts)
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
	s.annotateLiveness(ctx, st, out.Facts)
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
