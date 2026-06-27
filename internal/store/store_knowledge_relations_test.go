package store

import (
	"strings"
	"testing"
)

func TestValidEdgeKind(t *testing.T) {
	valid := []string{"DependsOn", "RelatedTo", "Supports", "Contradicts", "Supersedes"}
	for _, k := range valid {
		if !validEdgeKind(k) {
			t.Errorf("validEdgeKind(%q) = false, want true", k)
		}
	}
	invalid := []string{"", "depends", "RELATEDTO", "unknown", "DependsOn "}
	for _, k := range invalid {
		if validEdgeKind(k) {
			t.Errorf("validEdgeKind(%q) = true, want false", k)
		}
	}
}

func TestKnowledgeRelate_Create(t *testing.T) {
	st, ctx := newStore(t)
	if _, err := st.KnowledgeAdd(ctx, "Gotcha", "fact alpha", 0.9); err != nil {
		t.Fatal(err)
	}
	if _, err := st.KnowledgeAdd(ctx, "Gotcha", "fact beta", 0.9); err != nil {
		t.Fatal(err)
	}
	var id1, id2 int64
	if err := st.db.QueryRowContext(ctx, `SELECT id FROM knowledge_facts WHERE body=?`, "fact alpha").Scan(&id1); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRowContext(ctx, `SELECT id FROM knowledge_facts WHERE body=?`, "fact beta").Scan(&id2); err != nil {
		t.Fatal(err)
	}

	if err := st.KnowledgeRelate(ctx, id1, id2, "Supports"); err != nil {
		t.Fatalf("KnowledgeRelate: %v", err)
	}

	rels, err := st.KnowledgeRelations(ctx, id1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rels) != 1 {
		t.Fatalf("expected 1 relation, got %d", len(rels))
	}
	if rels[0].FromID != id1 || rels[0].ToID != id2 || rels[0].Kind != "Supports" {
		t.Errorf("unexpected relation: %+v", rels[0])
	}
}

func TestKnowledgeRelate_Reinforce(t *testing.T) {
	st, ctx := newStore(t)
	_, _ = st.KnowledgeAdd(ctx, "Gotcha", "fact one", 0.9)
	_, _ = st.KnowledgeAdd(ctx, "Gotcha", "fact two", 0.9)
	var id1, id2 int64
	_ = st.db.QueryRowContext(ctx, `SELECT id FROM knowledge_facts WHERE body='fact one'`).Scan(&id1)
	_ = st.db.QueryRowContext(ctx, `SELECT id FROM knowledge_facts WHERE body='fact two'`).Scan(&id2)

	_ = st.KnowledgeRelate(ctx, id1, id2, "RelatedTo")
	_ = st.KnowledgeRelate(ctx, id1, id2, "RelatedTo")

	rels, err := st.KnowledgeRelations(ctx, id1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rels) != 1 {
		t.Fatalf("expected 1 relation after reinforce, got %d", len(rels))
	}
	if rels[0].Count != 2 {
		t.Errorf("expected Count=2 after reinforce, got %d", rels[0].Count)
	}
	if rels[0].Strength > 1.0 {
		t.Errorf("strength exceeds 1.0: %f", rels[0].Strength)
	}
}

func TestKnowledgeRelate_SelfLoop(t *testing.T) {
	st, ctx := newStore(t)
	_, _ = st.KnowledgeAdd(ctx, "Gotcha", "fact self", 0.9)
	var id int64
	_ = st.db.QueryRowContext(ctx, `SELECT id FROM knowledge_facts WHERE body='fact self'`).Scan(&id)
	err := st.KnowledgeRelate(ctx, id, id, "RelatedTo")
	if err == nil {
		t.Error("expected error for self-loop, got nil")
	}
}

func TestKnowledgeRelate_UnknownKind(t *testing.T) {
	st, ctx := newStore(t)
	_, _ = st.KnowledgeAdd(ctx, "Gotcha", "fact a", 0.9)
	_, _ = st.KnowledgeAdd(ctx, "Gotcha", "fact b", 0.9)
	var id1, id2 int64
	_ = st.db.QueryRowContext(ctx, `SELECT id FROM knowledge_facts WHERE body='fact a'`).Scan(&id1)
	_ = st.db.QueryRowContext(ctx, `SELECT id FROM knowledge_facts WHERE body='fact b'`).Scan(&id2)
	err := st.KnowledgeRelate(ctx, id1, id2, "Invented")
	if err == nil {
		t.Error("expected error for unknown edge kind, got nil")
	}
}

func TestKnowledgeRelate_MissingFact(t *testing.T) {
	st, ctx := newStore(t)
	_, _ = st.KnowledgeAdd(ctx, "Gotcha", "fact a", 0.9)
	var id int64
	_ = st.db.QueryRowContext(ctx, `SELECT id FROM knowledge_facts WHERE body='fact a'`).Scan(&id)
	err := st.KnowledgeRelate(ctx, id, 99999, "Supports")
	if err == nil {
		t.Error("expected error for missing fact id, got nil")
	}
}

func TestKnowledgeRelations_Empty(t *testing.T) {
	st, ctx := newStore(t)
	_, _ = st.KnowledgeAdd(ctx, "Gotcha", "isolated fact", 0.9)
	var id int64
	_ = st.db.QueryRowContext(ctx, `SELECT id FROM knowledge_facts WHERE body='isolated fact'`).Scan(&id)
	rels, err := st.KnowledgeRelations(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(rels) != 0 {
		t.Errorf("expected 0 relations, got %d", len(rels))
	}
}

func TestKnowledgeRelations_BothDirections(t *testing.T) {
	st, ctx := newStore(t)
	_, _ = st.KnowledgeAdd(ctx, "Gotcha", "fact A", 0.9)
	_, _ = st.KnowledgeAdd(ctx, "Gotcha", "fact B", 0.9)
	_, _ = st.KnowledgeAdd(ctx, "Gotcha", "fact C", 0.9)
	var id1, id2, id3 int64
	_ = st.db.QueryRowContext(ctx, `SELECT id FROM knowledge_facts WHERE body='fact A'`).Scan(&id1)
	_ = st.db.QueryRowContext(ctx, `SELECT id FROM knowledge_facts WHERE body='fact B'`).Scan(&id2)
	_ = st.db.QueryRowContext(ctx, `SELECT id FROM knowledge_facts WHERE body='fact C'`).Scan(&id3)

	_ = st.KnowledgeRelate(ctx, id1, id2, "DependsOn")
	_ = st.KnowledgeRelate(ctx, id3, id1, "Supports")

	rels, err := st.KnowledgeRelations(ctx, id1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rels) != 2 {
		t.Errorf("expected 2 relations (1 outgoing, 1 incoming), got %d", len(rels))
	}
}

func TestKnowledgeRelationDiagram_Empty(t *testing.T) {
	st, ctx := newStore(t)
	got, err := st.KnowledgeRelationDiagram(ctx, 0.0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "no relations") {
		t.Errorf("expected empty-diagram hint, got %q", got)
	}
}

func TestKnowledgeRelationDiagram_WithEdge(t *testing.T) {
	st, ctx := newStore(t)
	_, _ = st.KnowledgeAdd(ctx, "Gotcha", "fact alpha", 0.9)
	_, _ = st.KnowledgeAdd(ctx, "Gotcha", "fact beta", 0.9)
	var id1, id2 int64
	_ = st.db.QueryRowContext(ctx, `SELECT id FROM knowledge_facts WHERE body='fact alpha'`).Scan(&id1)
	_ = st.db.QueryRowContext(ctx, `SELECT id FROM knowledge_facts WHERE body='fact beta'`).Scan(&id2)
	_ = st.KnowledgeRelate(ctx, id1, id2, "Supersedes")

	got, err := st.KnowledgeRelationDiagram(ctx, 0.0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "graph LR") {
		t.Errorf("expected Mermaid diagram, got %q", got)
	}
	if !strings.Contains(got, "Supersedes") {
		t.Errorf("expected edge kind in diagram, got %q", got)
	}
}

func TestKnowledgeRelationDiagram_StrengthFilter(t *testing.T) {
	st, ctx := newStore(t)
	_, _ = st.KnowledgeAdd(ctx, "Gotcha", "fact x", 0.9)
	_, _ = st.KnowledgeAdd(ctx, "Gotcha", "fact y", 0.9)
	var id1, id2 int64
	_ = st.db.QueryRowContext(ctx, `SELECT id FROM knowledge_facts WHERE body='fact x'`).Scan(&id1)
	_ = st.db.QueryRowContext(ctx, `SELECT id FROM knowledge_facts WHERE body='fact y'`).Scan(&id2)
	_ = st.KnowledgeRelate(ctx, id1, id2, "RelatedTo") // strength = 1.0

	// threshold above initial strength: no edges in diagram
	got, err := st.KnowledgeRelationDiagram(ctx, 1.1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "no relations") {
		t.Errorf("expected empty diagram above threshold, got %q", got)
	}

	// threshold at or below initial strength: edge appears
	got2, err := st.KnowledgeRelationDiagram(ctx, 1.0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got2, "RelatedTo") {
		t.Errorf("expected edge in diagram at threshold 1.0, got %q", got2)
	}
}

func TestExcerpt(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"short", 40, "short"},
		{"hello world this is a longer string that exceeds forty chars", 40, "hello world this is a longer string…"},
		{"abcdefghij", 5, "abcde…"},
	}
	for _, c := range cases {
		got := excerpt(c.in, c.n)
		if got != c.want {
			t.Errorf("excerpt(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}

func TestMermaidEsc(t *testing.T) {
	got := mermaidEsc(`it "quoted" me`)
	if strings.Contains(got, `"`) {
		t.Errorf("mermaidEsc left double-quote in: %q", got)
	}
	if !strings.Contains(got, `'`) {
		t.Errorf("mermaidEsc should replace \" with ': %q", got)
	}
}
