package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// KnowledgeEdgeKind are the valid relationship types between facts (#621).
// DependsOn: A cannot be understood without B.
// RelatedTo: A and B address the same area (weak, non-directional).
// Supports: A provides evidence for B.
// Contradicts: A and B are in tension — both survive, but the reader must pick.
// Supersedes: A replaces B (B is inactive; this edge records why).
type KnowledgeEdgeKind string

const (
	EdgeDependsOn   KnowledgeEdgeKind = "DependsOn"
	EdgeRelatedTo   KnowledgeEdgeKind = "RelatedTo"
	EdgeSupports    KnowledgeEdgeKind = "Supports"
	EdgeContradicts KnowledgeEdgeKind = "Contradicts"
	EdgeSupersedes  KnowledgeEdgeKind = "Supersedes"
)

// validEdgeKind returns true for recognized edge types.
func validEdgeKind(kind string) bool {
	switch KnowledgeEdgeKind(kind) {
	case EdgeDependsOn, EdgeRelatedTo, EdgeSupports, EdgeContradicts, EdgeSupersedes:
		return true
	}
	return false
}

// KnowledgeRelation is one directed, typed edge between two facts.
type KnowledgeRelation struct {
	FromID    int64
	ToID      int64
	Kind      KnowledgeEdgeKind
	Strength  float64 // saturates at 1.0 via Hebbian update
	Count     int     // times the edge was explicitly reinforced
	CreatedAt time.Time
}

// KnowledgeRelate creates an edge from→to of the given kind, or reinforces
// it with a Hebbian update if it already exists. Both IDs must refer to
// existing facts. Returns an error if either fact is missing or if kind is
// unrecognized.
func (s *knowledgeStore) KnowledgeRelate(ctx context.Context, fromID, toID int64, kind string) error {
	if !validEdgeKind(kind) {
		return fmt.Errorf("unknown edge kind %q: must be DependsOn|RelatedTo|Supports|Contradicts|Supersedes", kind)
	}
	if fromID == toID {
		return fmt.Errorf("self-loops not allowed: from_id == to_id == %d", fromID)
	}

	// Verify both facts exist (either active or inactive — we allow edges on
	// inactive facts so Supersedes edges survive after supersession).
	for _, id := range []int64{fromID, toID} {
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM knowledge_facts WHERE id=?`, id).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("fact #%d not found", id)
		}
	}

	now := time.Now().UnixNano()
	// Try INSERT; on conflict (same from/to/kind) apply Hebbian update.
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO knowledge_relations(from_id, to_id, kind, strength, count, created_at)
		   VALUES(?,?,?,1.0,1,?)
		   ON CONFLICT(from_id, to_id, kind) DO UPDATE SET
		     strength = MIN(strength + 0.1*(1.0-strength), 1.0),
		     count    = count + 1`,
		fromID, toID, kind, now)
	return err
}

// KnowledgeRelations lists all edges connected to the given fact (both
// outgoing and incoming), sorted by strength descending. Returns an empty
// slice if the fact has no edges (not an error).
func (s *knowledgeStore) KnowledgeRelations(ctx context.Context, id int64) ([]KnowledgeRelation, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT from_id, to_id, kind, strength, count, created_at
		   FROM knowledge_relations
		   WHERE from_id=? OR to_id=?
		   ORDER BY strength DESC, count DESC`,
		id, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []KnowledgeRelation
	for rows.Next() {
		var r KnowledgeRelation
		var createdNs int64
		var kind string
		if err := rows.Scan(&r.FromID, &r.ToID, &kind, &r.Strength, &r.Count, &createdNs); err != nil {
			return nil, err
		}
		r.Kind = KnowledgeEdgeKind(kind)
		r.CreatedAt = time.Unix(0, createdNs)
		out = append(out, r)
	}
	return out, rows.Err()
}

// KnowledgeRelationDiagram returns a Mermaid graph of edges whose strength
// is at or above minStrength (0.0 = all edges). Nodes are labelled with the
// fact id and a short excerpt of the body; edges carry their kind and
// strength.
func (s *knowledgeStore) KnowledgeRelationDiagram(ctx context.Context, minStrength float64) (string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT r.from_id, r.to_id, r.kind, r.strength,
		        f1.body, f2.body
		   FROM knowledge_relations r
		   JOIN knowledge_facts f1 ON f1.id = r.from_id
		   JOIN knowledge_facts f2 ON f2.id = r.to_id
		   WHERE r.strength >= ?
		   ORDER BY r.strength DESC`,
		minStrength)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()

	type edge struct {
		fromID, toID       int64
		kind, body1, body2 string
		strength           float64
	}
	var edges []edge
	seen := map[int64]string{} // id → label
	for rows.Next() {
		var e edge
		if err := rows.Scan(&e.fromID, &e.toID, &e.kind, &e.strength, &e.body1, &e.body2); err != nil {
			return "", err
		}
		edges = append(edges, e)
		if _, ok := seen[e.fromID]; !ok {
			seen[e.fromID] = fmt.Sprintf("#%d: %s", e.fromID, excerpt(e.body1, 40))
		}
		if _, ok := seen[e.toID]; !ok {
			seen[e.toID] = fmt.Sprintf("#%d: %s", e.toID, excerpt(e.body2, 40))
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if len(edges) == 0 {
		return "graph LR\n  %% no relations above threshold", nil
	}

	var sb strings.Builder
	sb.WriteString("graph LR\n")
	for id, label := range seen {
		fmt.Fprintf(&sb, "  n%d[\"%s\"]\n", id, mermaidEsc(label))
	}
	for _, e := range edges {
		fmt.Fprintf(&sb, "  n%d -->|%s %.2f| n%d\n", e.fromID, e.kind, e.strength, e.toID)
	}
	return sb.String(), nil
}

// excerpt returns at most n runes of s, trimming at a word boundary when
// possible, and appending "…" when truncated.
func excerpt(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	cut := string(runes[:n])
	if i := strings.LastIndexByte(cut, ' '); i > n/2 {
		cut = cut[:i]
	}
	return cut + "…"
}

// mermaidEsc escapes double-quotes for Mermaid node labels.
func mermaidEsc(s string) string {
	return strings.ReplaceAll(s, `"`, `'`)
}
