package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	pathpkg "path"
	"sort"
	"strings"
	"time"
)

// knowledgeStore holds knowledge-fact methods, embedded in Store.
type knowledgeStore struct{ db *sql.DB }

// KnowledgeFact is one persisted fact about the project.
type KnowledgeFact struct {
	ID            int64
	Archetype     string // Architecture | Gotcha | Convention | Decision | Observation | Dependency | Pattern | Fact
	Body          string
	Confidence    float64 // 0–1
	CreatedAt     time.Time
	UpdatedAt     time.Time
	HitCount      int
	RevisionCount int
	Salience      float64 // pre-computed: confidence * archetypeWeight * recency
	// Scope optionally binds a fact to a file glob / path / package (#645), so a
	// file verb can surface it when it touches a matching path. Empty = unscoped
	// (the default; surfaces only through query-time recall). Populated only by
	// KnowledgeByScope; other reads leave it empty.
	Scope string
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
// updated_at without creating a duplicate. Returns the revision_count after
// insert/update (0 = first time stored, 1 = first revision, etc.).
func (s *knowledgeStore) KnowledgeAdd(ctx context.Context, archetype, body string, confidence float64) (int, error) {
	return s.KnowledgeAddScoped(ctx, archetype, body, confidence, "")
}

// KnowledgeAddScoped is KnowledgeAdd with an optional `scope` (#645) — a file
// glob / path / package the fact is about, so a file verb can surface it on
// touch. Empty scope behaves exactly like KnowledgeAdd. On a body conflict the
// scope is updated too (re-adding with a scope binds an existing fact).
func (s *knowledgeStore) KnowledgeAddScoped(ctx context.Context, archetype, body string, confidence float64, scope string) (int, error) {
	if confidence <= 0 {
		confidence = 0.8
	}
	if confidence > 1 {
		confidence = 1
	}
	now := time.Now().UnixNano()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO knowledge_facts(archetype, body, confidence, created_at, updated_at, hit_count, revision_count, scope)
		   VALUES(?,?,?,?,?,0,0,?)
		   ON CONFLICT(body) DO UPDATE SET
		     archetype=excluded.archetype,
		     confidence=MAX(confidence, excluded.confidence),
		     updated_at=excluded.updated_at,
		     revision_count=revision_count+1,
		     scope=CASE WHEN excluded.scope != '' THEN excluded.scope ELSE scope END`,
		archetype, body, confidence, now, now, scope)
	if err != nil {
		return 0, err
	}
	var rev int
	_ = s.db.QueryRowContext(ctx, `SELECT revision_count FROM knowledge_facts WHERE body=?`, body).Scan(&rev)
	return rev, nil
}

// KnowledgeByScope returns the scoped facts whose scope matches targetPath
// (#645) — a file glob (`internal/mcp/*_test.go`), a directory/package prefix
// (`internal/mcp`), or an exact path — salience-ranked, capped at max. Unscoped
// facts (scope=”) are never returned. Powers the proactive "gotcha-on-touch"
// surfacing in file verbs.
func (s *knowledgeStore) KnowledgeByScope(ctx context.Context, targetPath string, max int) ([]KnowledgeFact, error) {
	if strings.TrimSpace(targetPath) == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, archetype, body, confidence, created_at, updated_at, hit_count, revision_count, scope
		   FROM knowledge_facts WHERE scope != ''`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []KnowledgeFact
	for rows.Next() {
		var f KnowledgeFact
		var cNs, uNs int64
		if err := rows.Scan(&f.ID, &f.Archetype, &f.Body, &f.Confidence, &cNs, &uNs, &f.HitCount, &f.RevisionCount, &f.Scope); err != nil {
			return nil, err
		}
		f.CreatedAt = time.Unix(0, cNs)
		f.UpdatedAt = time.Unix(0, uNs)
		f.Salience = qualityWeight(f) * recencyFactor(f.UpdatedAt)
		if scopeMatches(f.Scope, targetPath) {
			out = append(out, f)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Salience > out[j].Salience })
	if max > 0 && len(out) > max {
		out = out[:max]
	}
	return out, nil
}

// scopeMatches reports whether a fact's scope binds targetPath. A scope with
// glob metacharacters is matched (path.Match) against the full path and the
// basename, so `*_test.go` and `internal/mcp/*_test.go` both work; a plain scope
// matches an exact path or a directory/package prefix.
func scopeMatches(scope, targetPath string) bool {
	scope = strings.TrimSpace(scope)
	if scope == "" || targetPath == "" {
		return false
	}
	if strings.ContainsAny(scope, "*?[") {
		if ok, _ := pathpkg.Match(scope, targetPath); ok {
			return true
		}
		if ok, _ := pathpkg.Match(scope, pathpkg.Base(targetPath)); ok {
			return true
		}
		return false
	}
	return targetPath == scope || strings.HasPrefix(targetPath, scope+"/")
}

// recencyFactor is a linear 90-day decay in [0,1]: a fact confirmed today
// scores 1.0, one confirmed ≥90 days ago scores 0.0. Mirrors lean-ctx's
// recency_decay. Uses wall-clock time — acceptable for ranking (a backward
// clock step only nudges relative ordering, never correctness).
func recencyFactor(updatedAt time.Time) float64 {
	days := time.Since(updatedAt).Hours() / 24
	if days <= 0 {
		return 1
	}
	r := 1 - days/90
	if r < 0 {
		return 0
	}
	return r
}

// qualityWeight is the query-independent salience signal:
// confidence × archetype weight (max ≈ 1.5).
func qualityWeight(f KnowledgeFact) float64 {
	return f.Confidence * archetypeWeight(f.Archetype)
}

// scanFacts reads KnowledgeFact rows in the column order used by the queries
// below and fills the recency-aware Salience field.
func scanFacts(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]KnowledgeFact, error) {
	var out []KnowledgeFact
	for rows.Next() {
		var f KnowledgeFact
		var cNs, uNs int64
		if err := rows.Scan(&f.ID, &f.Archetype, &f.Body, &f.Confidence, &cNs, &uNs, &f.HitCount, &f.RevisionCount); err != nil {
			return nil, err
		}
		f.CreatedAt = time.Unix(0, cNs)
		f.UpdatedAt = time.Unix(0, uNs)
		f.Salience = qualityWeight(f) * recencyFactor(f.UpdatedAt)
		out = append(out, f)
	}
	return out, rows.Err()
}

// clampK normalizes a requested result count to [1,50] with a default of 10.
func clampK(k int) int {
	if k <= 0 {
		return 10
	}
	if k > 50 {
		return 50
	}
	return k
}

// KnowledgeQuery returns the top-k facts ordered by salience
// (confidence × archetype weight × recency decay).
// Pass k<=0 for the default (10).
func (s *knowledgeStore) KnowledgeQuery(ctx context.Context, k int) ([]KnowledgeFact, error) {
	k = clampK(k)
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, archetype, body, confidence, created_at, updated_at, hit_count, revision_count
		   FROM knowledge_facts
		   ORDER BY confidence DESC, updated_at DESC
		   LIMIT ?`, k)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanFacts(rows)
}

// SimilarFact pairs a stored fact with its Jaccard word-overlap against a
// candidate body, in [0,1].
type SimilarFact struct {
	KnowledgeFact
	Similarity float64
}

// KnowledgeSimilar returns stored facts whose body word-set overlaps the
// candidate body at or above threshold — the same Jaccard metric the GC merge
// pass uses — best-match first, capped at max (0 = no cap). Byte-identical
// bodies are excluded: those are the KnowledgeAdd upsert path, not a
// near-duplicate. It is the write-time companion to the GC's after-the-fact
// merge, surfaced on `notes add` so the author can supersede instead of
// silently duplicating or contradicting an existing fact (#606).
func (s *knowledgeStore) KnowledgeSimilar(ctx context.Context, body string, threshold float64, max int) ([]SimilarFact, error) {
	cand := wordSet(body)
	if len(cand) == 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, archetype, body, confidence, created_at, updated_at, hit_count, revision_count
		   FROM knowledge_facts`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	facts, err := scanFacts(rows)
	if err != nil {
		return nil, err
	}
	var out []SimilarFact
	for _, f := range facts {
		if f.Body == body {
			continue // exact dup → KnowledgeAdd upsert handles it
		}
		if sim := jaccard(cand, wordSet(f.Body)); sim >= threshold {
			out = append(out, SimilarFact{KnowledgeFact: f, Similarity: sim})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Similarity > out[j].Similarity })
	if max > 0 && len(out) > max {
		out = out[:max]
	}
	return out, nil
}

// KnowledgeExportAll returns every stored fact, UNCAPPED (KnowledgeQuery clamps
// to 50), newest-confident first. For backup/restore — notably preserving the
// knowledge store across a `dex reindex` that drops and rebuilds the DB, so the
// user's accumulated notes survive (#647).
func (s *knowledgeStore) KnowledgeExportAll(ctx context.Context) ([]KnowledgeFact, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, archetype, body, confidence, created_at, updated_at, hit_count, revision_count
		   FROM knowledge_facts
		   ORDER BY confidence DESC, updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanFacts(rows)
}

// KnowledgeBackup is the portable note shape used to rescue notes across a
// reindex — including one triggered BY a schema-version mismatch (#648). It
// round-trips every field that survives a rebuild: the scope binding (#645) and
// the created_at / updated_at / hit_count / revision_count that feed Salience.
// Dropping these on restore silently unscopes notes and resets their ranking
// signal (#678).
type KnowledgeBackup struct {
	Archetype     string
	Body          string
	Confidence    float64
	Scope         string
	CreatedAt     int64 // unix nanos; 0 when the source schema predates the column
	UpdatedAt     int64 // unix nanos; 0 when the source schema predates the column
	HitCount      int
	RevisionCount int
}

// rawColumns returns the set of column names on table, read straight from
// sqlite_master via PRAGMA — no migrations. Used to make a raw export tolerant
// of older schemas that lack newer columns (e.g. a pre-#645 DB has no `scope`).
func rawColumns(ctx context.Context, db *sql.DB, table string) map[string]bool {
	rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()
	cols := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return cols
		}
		cols[name] = true
	}
	return cols
}

// ExportKnowledgeRaw reads all notes directly from a sqlite index file WITHOUT
// running migrations, so notes survive even a reindex caused by a schema-version
// mismatch (when the normal store.Open fail-closes). archetype/body/confidence
// have existed in every schemaVersion; the newer columns (scope, created_at,
// updated_at, hit_count, revision_count) are selected only when present, so the
// read stays version-independent and forward-tolerant (#678). The sqlite-vec
// extension is auto-registered at package init, so a DB carrying a vec0 table
// still opens. Best-effort: returns nil (no error) when the file or the
// knowledge_facts table is absent/unreadable.
func ExportKnowledgeRaw(ctx context.Context, dbPath string) ([]KnowledgeBackup, error) {
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, nil
	}
	defer func() { _ = db.Close() }()
	cols := rawColumns(ctx, db, "knowledge_facts")
	if !cols["archetype"] {
		return nil, nil // table missing or unreadable → nothing to rescue
	}
	// Build a fixed 8-column projection: present columns select directly, absent
	// ones fall back to a literal default so the scan signature never changes.
	col := func(name, def string) string {
		if cols[name] {
			return name
		}
		return def + " AS " + name
	}
	query := `SELECT archetype, body, confidence, ` +
		col("scope", "''") + `, ` +
		col("created_at", "0") + `, ` +
		col("updated_at", "0") + `, ` +
		col("hit_count", "0") + `, ` +
		col("revision_count", "0") +
		` FROM knowledge_facts ORDER BY id`
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, nil // file/table missing or unreadable → nothing to rescue
	}
	defer func() { _ = rows.Close() }()
	var out []KnowledgeBackup
	for rows.Next() {
		var b KnowledgeBackup
		if err := rows.Scan(&b.Archetype, &b.Body, &b.Confidence, &b.Scope,
			&b.CreatedAt, &b.UpdatedAt, &b.HitCount, &b.RevisionCount); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// KnowledgeRestore re-inserts a rescued fact verbatim, preserving the scope
// binding and the created_at / updated_at / hit_count / revision_count that feed
// Salience — unlike KnowledgeAdd, which stamps a fresh created_at and zeroes the
// counters. Only the reindex rescue path uses it (#678). On a body conflict
// (idempotent re-restore) it keeps the stronger signal: higher confidence, a
// non-empty scope, and the larger counters. A zero CreatedAt/UpdatedAt (source
// predated the column) is stamped to now so recency stays sane.
func (s *knowledgeStore) KnowledgeRestore(ctx context.Context, b KnowledgeBackup) error {
	if b.Confidence <= 0 {
		b.Confidence = 0.8
	}
	if b.Confidence > 1 {
		b.Confidence = 1
	}
	now := time.Now().UnixNano()
	if b.CreatedAt == 0 {
		b.CreatedAt = now
	}
	if b.UpdatedAt == 0 {
		b.UpdatedAt = b.CreatedAt
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO knowledge_facts(archetype, body, confidence, created_at, updated_at, hit_count, revision_count, scope)
		   VALUES(?,?,?,?,?,?,?,?)
		   ON CONFLICT(body) DO UPDATE SET
		     archetype=excluded.archetype,
		     confidence=MAX(confidence, excluded.confidence),
		     updated_at=MAX(updated_at, excluded.updated_at),
		     hit_count=MAX(hit_count, excluded.hit_count),
		     revision_count=MAX(revision_count, excluded.revision_count),
		     scope=CASE WHEN excluded.scope != '' THEN excluded.scope ELSE scope END`,
		b.Archetype, b.Body, b.Confidence, b.CreatedAt, b.UpdatedAt, b.HitCount, b.RevisionCount, b.Scope)
	return err
}

// KnowledgeCount returns the number of stored facts.
func (s *knowledgeStore) KnowledgeCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM knowledge_facts`).Scan(&n)
	return n, err
}

// KnowledgeDelete removes a fact by id.
func (s *knowledgeStore) KnowledgeDelete(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM knowledge_facts WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("fact not found")
	}
	return nil
}

// KnowledgeBump increments the hit_count for a fact and records the retrieval
// time (called when a fact is surfaced to an agent). It deliberately does NOT
// touch updated_at — that stays the "last confirmed" timestamp, so decay
// (KnowledgeGC) measures staleness from confirmation while protecting facts
// that are still being retrieved.
func (s *knowledgeStore) KnowledgeBump(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE knowledge_facts SET hit_count=hit_count+1, last_retrieved=? WHERE id=?`,
		time.Now().UnixNano(), id)
	return err
}

// KnowledgeTopForAsk returns up to k high-salience facts to inject into
// ask context. It also bumps the hit_count for every returned fact.
func (s *knowledgeStore) KnowledgeTopForAsk(ctx context.Context, k int) ([]KnowledgeFact, error) {
	facts, err := s.KnowledgeQuery(ctx, k)
	if err != nil || len(facts) == 0 {
		return facts, err
	}
	for _, f := range facts {
		_ = s.KnowledgeBump(ctx, f.ID)
	}
	return facts, nil
}

// ─── semantic recall (vec0-backed) ──────────────────────────────────────────

// ensureFactVecTable materializes the fact_vecs vec0 virtual table at the given
// embedding dimension and the delete-cascade trigger that drops a fact's vector
// when the fact is removed. Idempotent. If a fact_vecs table already exists at a
// different dimension (e.g. the embed model changed), it is dropped and
// recreated — the embeddings are re-backfilled lazily on the next recall.
func (s *knowledgeStore) ensureFactVecTable(ctx context.Context, dim int) error {
	if dim <= 0 {
		return nil
	}
	var recorded string
	_ = s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='fact_vecs_dim'`).Scan(&recorded)
	want := fmt.Sprintf("%d", dim)
	if recorded != "" && recorded != want {
		if _, err := s.db.ExecContext(ctx, `DROP TABLE IF EXISTS fact_vecs`); err != nil {
			return fmt.Errorf("ensure fact vec table: drop on dim change: %w", err)
		}
	}
	stmts := []string{
		fmt.Sprintf(`CREATE VIRTUAL TABLE IF NOT EXISTS fact_vecs USING vec0(
		   embedding FLOAT[%d] distance_metric=cosine
		 )`, dim),
		`CREATE TRIGGER IF NOT EXISTS knowledge_facts_vec_ad AFTER DELETE ON knowledge_facts BEGIN
		   DELETE FROM fact_vecs WHERE rowid = old.id;
		 END`,
	}
	for _, q := range stmts {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("ensure fact vec table: %w", err)
		}
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO meta(key, value) VALUES('fact_vecs_dim', ?)
		   ON CONFLICT(key) DO UPDATE SET value=excluded.value`, want); err != nil {
		return fmt.Errorf("ensure fact vec table: record dim: %w", err)
	}
	return nil
}

// KnowledgeUpsertVec stores (or replaces) the embedding for a fact id in the
// fact_vecs table. The vec0 table is created on first use at the vector's
// dimension. A nil/empty vec is a no-op.
func (s *knowledgeStore) KnowledgeUpsertVec(ctx context.Context, id int64, vec []float32) error {
	if len(vec) == 0 {
		return nil
	}
	if err := s.ensureFactVecTable(ctx, len(vec)); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM fact_vecs WHERE rowid=?`, id); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO fact_vecs(rowid, embedding) VALUES(?, ?)`, id, encodeVec(vec))
	return err
}

// KnowledgeUpsertVecByBody resolves a fact id by its (unique) body and stores
// its embedding. Used right after KnowledgeAdd, which dedups by body.
func (s *knowledgeStore) KnowledgeUpsertVecByBody(ctx context.Context, body string, vec []float32) error {
	var id int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT id FROM knowledge_facts WHERE body=?`, body).Scan(&id); err != nil {
		return err
	}
	return s.KnowledgeUpsertVec(ctx, id, vec)
}

// KnowledgeFactsMissingVec returns up to limit facts that have no row in
// fact_vecs (never embedded, or embedded under a now-dropped dimension). When
// the fact_vecs table does not exist yet, every fact is considered missing.
// Callers embed these bodies and feed them back through KnowledgeUpsertVec.
func (s *knowledgeStore) KnowledgeFactsMissingVec(ctx context.Context, limit int) ([]KnowledgeFact, error) {
	if limit <= 0 {
		limit = 128
	}
	q := `SELECT id, archetype, body, confidence, created_at, updated_at, hit_count, revision_count
	        FROM knowledge_facts f
	       WHERE NOT EXISTS (SELECT 1 FROM fact_vecs v WHERE v.rowid = f.id)
	       ORDER BY updated_at DESC
	       LIMIT ?`
	rows, err := s.db.QueryContext(ctx, q, limit)
	if err != nil {
		// fact_vecs missing → treat all facts as missing.
		rows, err = s.db.QueryContext(ctx,
			`SELECT id, archetype, body, confidence, created_at, updated_at, hit_count, revision_count
			   FROM knowledge_facts ORDER BY updated_at DESC LIMIT ?`, limit)
		if err != nil {
			return nil, err
		}
	}
	defer func() { _ = rows.Close() }()
	return scanFacts(rows)
}

// KnowledgeQueryVec returns up to k facts ranked by a hybrid of semantic
// similarity to queryVec, query-independent quality (confidence × archetype
// weight), and recency. Weights mirror lean-ctx: 0.6 / 0.25 / 0.15.
//
// minSim sets a cosine-similarity floor: facts with sim < minSim are dropped
// before hybrid ranking. Pass 0 to disable (ask/list callers); pass ~0.25 for
// skip-fallback callers (locate/review) that should show no note rather than
// an irrelevant one when the knowledge base is sparse (#706).
//
// Falls back to salience-only KnowledgeQuery when queryVec is empty or no fact
// embeddings exist yet (fresh store, or embed endpoint was offline at add time).
func (s *knowledgeStore) KnowledgeQueryVec(ctx context.Context, queryVec []float32, k int, minSim float64) ([]KnowledgeFact, error) {
	k = clampK(k)
	if len(queryVec) == 0 {
		return s.KnowledgeQuery(ctx, k)
	}
	var cnt int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM fact_vecs`).Scan(&cnt); err != nil || cnt == 0 {
		return s.KnowledgeQuery(ctx, k) // table absent or empty → salience fallback
	}

	// Pull a candidate pool by cosine distance, wider than k so the hybrid
	// rerank can promote a slightly-less-similar but fresher/higher-quality fact.
	pool := k * 4
	if pool < 20 {
		pool = 20
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT rowid, distance FROM fact_vecs
		 WHERE embedding MATCH ? AND k = ?
		 ORDER BY distance`, encodeVec(queryVec), pool)
	if err != nil {
		return s.KnowledgeQuery(ctx, k)
	}
	sims := make(map[int64]float64)
	ids := make([]int64, 0, pool)
	for rows.Next() {
		var id int64
		var dist float64
		if err := rows.Scan(&id, &dist); err != nil {
			_ = rows.Close()
			return nil, err
		}
		sim := 1 - dist // cosine distance ∈ [0,2] → similarity ∈ [-1,1]
		if sim < minSim {
			continue // below similarity floor — skip before DB fetch (#706)
		}
		sims[id] = sim
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()
	if len(ids) == 0 {
		return nil, nil // all below floor; skip-fallback callers get empty, not salience noise
	}

	factQ := `SELECT id, archetype, body, confidence, created_at, updated_at, hit_count, revision_count FROM knowledge_facts WHERE id IN (` + inPlaceholders(len(ids)) + `)` //nolint:gosec // placeholder count is generated; ids passed as bind args
	frows, err := s.db.QueryContext(ctx, factQ, int64sToAny(ids)...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = frows.Close() }()
	facts, err := scanFacts(frows)
	if err != nil {
		return nil, err
	}

	const (
		wSemantic = 0.6
		wQuality  = 0.25
		wRecency  = 0.15
		maxWeight = 1.5 // max confidence(1) × max archetypeWeight(1.5)
	)
	type ranked struct {
		f     KnowledgeFact
		score float64
	}
	scoredFacts := make([]ranked, 0, len(facts))
	for _, f := range facts {
		sem := sims[f.ID]
		if sem < 0 {
			sem = 0
		}
		q := qualityWeight(f) / maxWeight
		score := wSemantic*sem + wQuality*q + wRecency*recencyFactor(f.UpdatedAt)
		scoredFacts = append(scoredFacts, ranked{f, score})
	}
	sort.SliceStable(scoredFacts, func(i, j int) bool { return scoredFacts[i].score > scoredFacts[j].score })
	if len(scoredFacts) > k {
		scoredFacts = scoredFacts[:k]
	}
	out := make([]KnowledgeFact, len(scoredFacts))
	for i, r := range scoredFacts {
		out[i] = r.f
	}
	return out, nil
}

// int64sToAny boxes an []int64 for use as variadic query args.
func int64sToAny(xs []int64) []any {
	out := make([]any, len(xs))
	for i, x := range xs {
		out[i] = x
	}
	return out
}

// ─── lifecycle: decay, consolidation, eviction (#225) ───────────────────────

// KnowledgeGCConfig tunes a knowledge-store maintenance pass. Zero values fall
// back to the documented defaults.
type KnowledgeGCConfig struct {
	DecayPerDay     float64 // confidence lost per elapsed day since the last GC, before protection (default 0.01)
	ConfidenceFloor float64 // decay never pushes confidence below this (default 0.05)
	JaccardMerge    float64 // word-overlap ≥ this merges two facts of the same archetype (default 0.85)
	MaxFacts        int     // evict lowest-confidence facts beyond this cap (default 1000; ≤0 keeps the default; negative disables)
}

func (c *KnowledgeGCConfig) applyDefaults() {
	if c.DecayPerDay <= 0 {
		c.DecayPerDay = 0.01
	}
	if c.ConfidenceFloor <= 0 {
		c.ConfidenceFloor = 0.05
	}
	if c.JaccardMerge <= 0 {
		c.JaccardMerge = 0.85
	}
	if c.MaxFacts == 0 {
		c.MaxFacts = 1000
	}
}

// KnowledgeGCResult reports what a maintenance pass changed.
type KnowledgeGCResult struct {
	Decayed   int `json:"decayed"`   // facts whose confidence was reduced
	Merged    int `json:"merged"`    // near-duplicate facts folded into another and deleted
	Evicted   int `json:"evicted"`   // facts dropped to satisfy the cap
	Remaining int `json:"remaining"` // facts left after the pass
}

// KnowledgeGC runs the fact-store lifecycle: confidence decay (protected by
// retrieval frequency/recency), consolidation of near-duplicate facts within an
// archetype, and eviction past a cap. It is safe to call repeatedly — decay is
// proportional to time elapsed since the previous GC (recorded in meta), so it
// never double-counts.
func (s *knowledgeStore) KnowledgeGC(ctx context.Context, cfg KnowledgeGCConfig) (KnowledgeGCResult, error) {
	cfg.applyDefaults()
	var res KnowledgeGCResult
	now := time.Now()

	decayed, err := s.knowledgeDecay(ctx, cfg, now)
	if err != nil {
		return res, err
	}
	res.Decayed = decayed

	merged, err := s.knowledgeConsolidateSimilar(ctx, cfg)
	if err != nil {
		return res, err
	}
	res.Merged = merged

	evicted, err := s.knowledgeEvict(ctx, cfg)
	if err != nil {
		return res, err
	}
	res.Evicted = evicted

	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM knowledge_facts`).Scan(&res.Remaining); err != nil {
		return res, err
	}
	return res, nil
}

// knowledgeDecay reduces each fact's confidence in proportion to the days
// elapsed since the last GC, divided by a protection factor that grows with
// hit_count, and further softened for facts retrieved within the last week.
// On the first ever GC it just records the baseline timestamp (no decay).
func (s *knowledgeStore) knowledgeDecay(ctx context.Context, cfg KnowledgeGCConfig, now time.Time) (int, error) {
	var lastGCStr string
	_ = s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='knowledge_last_gc'`).Scan(&lastGCStr)
	recordNow := func() error {
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO meta(key, value) VALUES('knowledge_last_gc', ?)
			   ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
			fmt.Sprintf("%d", now.UnixNano()))
		return err
	}
	if lastGCStr == "" {
		return 0, recordNow() // baseline only; nothing to decay against yet
	}
	var lastGCNs int64
	if _, err := fmt.Sscan(lastGCStr, &lastGCNs); err != nil {
		return 0, recordNow()
	}
	elapsedDays := now.Sub(time.Unix(0, lastGCNs)).Hours() / 24
	if elapsedDays <= 0 {
		return 0, nil // clock went backwards or no time passed; leave confidences and last_gc
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, confidence, hit_count, last_retrieved FROM knowledge_facts`)
	if err != nil {
		return 0, err
	}
	type decayRow struct {
		id      int64
		newConf float64
	}
	var updates []decayRow
	for rows.Next() {
		var id int64
		var conf float64
		var hitCount int
		var lastRetrieved int64
		if err := rows.Scan(&id, &conf, &hitCount, &lastRetrieved); err != nil {
			_ = rows.Close()
			return 0, err
		}
		protect := 1 + math.Log(1+float64(hitCount))
		decay := cfg.DecayPerDay * elapsedDays / protect
		if lastRetrieved > 0 && now.Sub(time.Unix(0, lastRetrieved)) < 7*24*time.Hour {
			decay *= 0.3 // recently useful → fade slower
		}
		newConf := conf - decay
		if newConf < cfg.ConfidenceFloor {
			newConf = cfg.ConfidenceFloor
		}
		if newConf < conf-1e-9 {
			updates = append(updates, decayRow{id, newConf})
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	_ = rows.Close()

	for _, u := range updates {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE knowledge_facts SET confidence=? WHERE id=?`, u.newConf, u.id); err != nil {
			return 0, err
		}
	}
	return len(updates), recordNow()
}

// knowledgeConsolidateSimilar merges facts within the same archetype whose body
// word-sets overlap at or above cfg.JaccardMerge. The higher-confidence fact
// survives, accumulating the other's hit_count; the duplicate is deleted (its
// fact_vecs row cascades away via trigger).
func (s *knowledgeStore) knowledgeConsolidateSimilar(ctx context.Context, cfg KnowledgeGCConfig) (int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, archetype, body, confidence, hit_count FROM knowledge_facts ORDER BY archetype, confidence DESC`)
	if err != nil {
		return 0, err
	}
	type fact struct {
		id        int64
		archetype string
		body      string
		conf      float64
		hits      int
		words     map[string]struct{}
	}
	var facts []fact
	for rows.Next() {
		var f fact
		if err := rows.Scan(&f.id, &f.archetype, &f.body, &f.conf, &f.hits); err != nil {
			_ = rows.Close()
			return 0, err
		}
		f.words = wordSet(f.body)
		facts = append(facts, f)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	_ = rows.Close()

	merged := 0
	deleted := make(map[int]bool)
	for i := range facts {
		if deleted[i] {
			continue
		}
		for j := i + 1; j < len(facts); j++ {
			if deleted[j] || facts[i].archetype != facts[j].archetype {
				continue
			}
			if jaccard(facts[i].words, facts[j].words) < cfg.JaccardMerge {
				continue
			}
			// facts are confidence-desc within archetype, so i is always the keeper.
			keeper, dup := &facts[i], &facts[j]
			if _, err := s.db.ExecContext(ctx,
				`UPDATE knowledge_facts SET hit_count=hit_count+?, confidence=MAX(confidence, ?) WHERE id=?`,
				dup.hits, dup.conf, keeper.id); err != nil {
				return merged, err
			}
			if err := s.KnowledgeDelete(ctx, dup.id); err != nil {
				return merged, err
			}
			deleted[j] = true
			merged++
		}
	}
	return merged, nil
}

// knowledgeEvict drops the lowest-confidence facts when the store exceeds the
// configured cap. Ties break toward the least-recently-retrieved.
func (s *knowledgeStore) knowledgeEvict(ctx context.Context, cfg KnowledgeGCConfig) (int, error) {
	if cfg.MaxFacts <= 0 {
		return 0, nil
	}
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM knowledge_facts`).Scan(&total); err != nil {
		return 0, err
	}
	excess := total - cfg.MaxFacts
	if excess <= 0 {
		return 0, nil
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM knowledge_facts WHERE id IN (
		   SELECT id FROM knowledge_facts
		   ORDER BY confidence ASC, last_retrieved ASC, updated_at ASC
		   LIMIT ?)`, excess)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// wordSet returns the set of lowercased alphanumeric tokens (length > 2) in s.
func wordSet(s string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	}) {
		if len(w) > 2 {
			out[w] = struct{}{}
		}
	}
	return out
}

// jaccard is the intersection-over-union of two word sets, in [0,1].
func jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for w := range a {
		if _, ok := b[w]; ok {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}
