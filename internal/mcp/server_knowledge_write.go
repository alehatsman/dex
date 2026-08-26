package mcp

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/store"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

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
	// #167 Part 3: referent-overlap supersede candidates — facts naming the same
	// file/symbol the word-overlap check misses. Skip when already superseding
	// (the author is resolving) and dedupe against the Jaccard hits above.
	var (
		refMatches []store.KnowledgeFact
		refShared  map[int64]string
	)
	if in.SupersedesID == 0 {
		excl := make(map[int64]bool, len(similar))
		for _, sf := range similar {
			excl[sf.ID] = true
		}
		refMatches, refShared = s.referentSupersedeMatches(ctx, st, in.Body, excl)
	}
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
	if len(refMatches) > 0 {
		anchors := make(map[string]bool)
		refOut := make([]KnowledgeFactOutput, 0, len(refMatches))
		for _, f := range refMatches {
			fo := knowledgeFactOut(f)
			refOut = append(refOut, fo)
			out.Similar = append(out.Similar, fo)
			for _, a := range strings.Split(refShared[f.ID], ", ") {
				anchors[a] = true
			}
		}
		names := make([]string, 0, len(anchors))
		for a := range anchors {
			names = append(names, a)
		}
		sort.Strings(names)
		out.Hint += fmt.Sprintf(" ⚠ %d note(s) already speak to %s (ids %s) — supersede rather than stack a contradiction.",
			len(refMatches), strings.Join(names, ", "), similarIDs(refOut))
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

// referentSupersedeMatches scans the active fact set for notes that name a code
// referent (file or symbol) in common with body but were not already surfaced as
// Jaccard word-overlap similar — supersede candidates the word check misses when
// the same anchor is worded differently (#167 Part 3). Advisory: the agent decides
// whether to supersede. Bounded by the ≤active-set scan (mirrors KnowledgeSimilar);
// best-effort, a store error yields no matches rather than failing the write.
func (s *Server) referentSupersedeMatches(ctx context.Context, st *store.Store, body string, exclude map[int64]bool) ([]store.KnowledgeFact, map[int64]string) {
	if len(referentKeys(extractReferents(body))) == 0 {
		return nil, nil
	}
	all, err := st.KnowledgeExportAll(ctx)
	if err != nil {
		return nil, nil
	}
	var matched []store.KnowledgeFact
	shared := make(map[int64]string)
	for _, f := range all {
		if f.Body == body || exclude[f.ID] {
			continue
		}
		hits := sharedReferents(body, f.Body)
		if len(hits) == 0 {
			continue
		}
		matched = append(matched, f)
		shared[f.ID] = strings.Join(hits, ", ")
		if len(matched) >= knowledgeReferentMax {
			break
		}
	}
	return matched, shared
}
