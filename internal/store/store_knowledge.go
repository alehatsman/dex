package store

import (
	"context"
	"errors"
	"time"
)

// KnowledgeFact is one persisted fact about the project.
type KnowledgeFact struct {
	ID         int64
	Archetype  string // Architecture | Gotcha | Convention | Decision | Observation | Dependency | Pattern | Fact
	Body       string
	Confidence float64 // 0–1
	CreatedAt  time.Time
	UpdatedAt  time.Time
	HitCount   int
	Salience   float64 // pre-computed: confidence * archetypeWeight * recency
}

// archetypeWeight returns the base salience weight for a known archetype.
func archetypeWeight(a string) float64 {
	switch a {
	case "Architecture":
		return 1.5
	case "Gotcha":
		return 1.4
	case "Decision":
		return 1.2
	case "Convention":
		return 1.1
	case "Dependency":
		return 1.1
	case "Pattern":
		return 1.0
	case "Fact":
		return 1.0
	default:
		return 1.0
	}
}

// KnowledgeAdd inserts or updates a fact. Facts are deduplicated by body text
// (case-sensitive). Updating an existing fact bumps its confidence and
// updated_at without creating a duplicate.
func (s *Store) KnowledgeAdd(ctx context.Context, archetype, body string, confidence float64) error {
	if confidence <= 0 {
		confidence = 0.8
	}
	if confidence > 1 {
		confidence = 1
	}
	now := time.Now().UnixNano()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO knowledge_facts(archetype, body, confidence, created_at, updated_at, hit_count)
		   VALUES(?,?,?,?,?,0)
		   ON CONFLICT(body) DO UPDATE SET
		     archetype=excluded.archetype,
		     confidence=MAX(confidence, excluded.confidence),
		     updated_at=excluded.updated_at`,
		archetype, body, confidence, now, now)
	return err
}

// KnowledgeQuery returns the top-k facts ordered by salience
// (confidence × archetype weight × recency decay).
// Pass k<=0 for the default (10).
func (s *Store) KnowledgeQuery(ctx context.Context, k int) ([]KnowledgeFact, error) {
	if k <= 0 {
		k = 10
	}
	if k > 50 {
		k = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, archetype, body, confidence, created_at, updated_at, hit_count
		   FROM knowledge_facts
		   ORDER BY confidence DESC, updated_at DESC
		   LIMIT ?`, k)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KnowledgeFact
	for rows.Next() {
		var f KnowledgeFact
		var cNs, uNs int64
		if err := rows.Scan(&f.ID, &f.Archetype, &f.Body, &f.Confidence, &cNs, &uNs, &f.HitCount); err != nil {
			return nil, err
		}
		f.CreatedAt = time.Unix(0, cNs)
		f.UpdatedAt = time.Unix(0, uNs)
		f.Salience = f.Confidence * archetypeWeight(f.Archetype)
		out = append(out, f)
	}
	return out, rows.Err()
}

// KnowledgeDelete removes a fact by id.
func (s *Store) KnowledgeDelete(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM knowledge_facts WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("fact not found")
	}
	return nil
}

// KnowledgeBump increments the hit_count for a fact (called when a fact is
// surfaced to an agent, so frequently-used facts stay salient).
func (s *Store) KnowledgeBump(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE knowledge_facts SET hit_count=hit_count+1, updated_at=? WHERE id=?`,
		time.Now().UnixNano(), id)
	return err
}

// KnowledgeTopForAsk returns up to k high-salience facts to inject into
// ask context. It also bumps the hit_count for every returned fact.
func (s *Store) KnowledgeTopForAsk(ctx context.Context, k int) ([]KnowledgeFact, error) {
	facts, err := s.KnowledgeQuery(ctx, k)
	if err != nil || len(facts) == 0 {
		return facts, err
	}
	for _, f := range facts {
		_ = s.KnowledgeBump(ctx, f.ID)
	}
	return facts, nil
}
