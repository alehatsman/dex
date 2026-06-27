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
	Archetype     string // Architecture | Gotcha | Convention | Decision | Observation | Dependency | Pattern | Fact | Hypothesis | Inference | VerifiedFact
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
	// Pinned marks a fact the author declared permanent (#633): exempt from
	// confidence decay, cap eviction, and `notes review` staleness proposals.
	// Populated only by reads that select it (KnowledgeReview); other reads
	// leave it false.
	Pinned bool
	// Active is false when the fact has been superseded by another (#606).
	// Inactive facts are excluded from all recall queries; they survive on disk
	// for audit. Populated only by KnowledgeReview; other reads always return
	// active facts.
	Active bool
	// SupersededBy is the id of the fact that replaced this one (#606). Zero
	// when not superseded. Populated only by KnowledgeReview.
	SupersededBy int64
	// Evidence marks facts derived from code inspection (#618). Evidence facts
	// decay at half the archetype's base rate, since code-level observations
	// are more durable than runtime hypotheses.
	Evidence bool
	// ValidUntil is the expiry time for time-bounded facts (#618). Zero means
	// no expiry. Facts past their ValidUntil are excluded from recall.
	ValidUntil time.Time
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
	case "VerifiedFact":
		return 1.6 // high-confidence, code-verified; also pinned-by-archetype from decay
	case "Inference":
		return 0.9 // derived; plausible but not directly observed
	case "Hypothesis":
		return 0.7 // unverified; evicts quickly via fast decay
	default:
		return 1.0
	}
}

// archetypeDecayRate returns the daily confidence decay rate for an archetype.
// Structural facts (Architecture, Decision) decay slowly; transient observations
// (Hypothesis) decay quickly. Evidence facts halve this rate at call-site.
func archetypeDecayRate(a string) float64 {
	switch a {
	case "Architecture":
		return 0.005
	case "Decision":
		return 0.006
	case "Convention":
		return 0.007
	case "Gotcha":
		return 0.008
	case "Dependency", "Pattern", "Fact":
		return 0.010
	case "VerifiedFact":
		return 0.003 // very slow — verified until explicitly superseded
	case "Inference":
		return 0.012
	case "Hypothesis":
		return 0.020 // fast — hypothesis ages out quickly if not confirmed
	default:
		return 0.010
	}
}

// KnowledgeAdd inserts or updates a fact. Facts are deduplicated by body text
// (case-sensitive). Updating an existing fact bumps its confidence and
// updated_at without creating a duplicate. Returns the revision_count after
// insert/update (0 = first time stored, 1 = first revision, etc.).
func (s *knowledgeStore) KnowledgeAdd(ctx context.Context, archetype, body string, confidence float64) (int, error) {
	return s.KnowledgeAddScoped(ctx, archetype, body, confidence, "")
}

// KnowledgeAddOpts carries optional fields for KnowledgeAddFull.
type KnowledgeAddOpts struct {
	Scope      string
	Evidence   bool
	ValidUntil time.Time // zero = no expiry
}

// KnowledgeAddScoped is KnowledgeAdd with an optional `scope` (#645) — a file
// glob / path / package the fact is about, so a file verb can surface it on
// touch. Empty scope behaves exactly like KnowledgeAdd. On a body conflict the
// scope is updated too (re-adding with a scope binds an existing fact).
func (s *knowledgeStore) KnowledgeAddScoped(ctx context.Context, archetype, body string, confidence float64, scope string) (int, error) {
	return s.KnowledgeAddFull(ctx, archetype, body, confidence, KnowledgeAddOpts{Scope: scope})
}

// KnowledgeAddFull is the full-featured add: scope, evidence flag, and
// valid_until expiry. Re-adding a superseded fact re-activates it
// (explicit re-confirmation beats prior supersession).
func (s *knowledgeStore) KnowledgeAddFull(ctx context.Context, archetype, body string, confidence float64, opts KnowledgeAddOpts) (int, error) {
	if confidence <= 0 {
		confidence = 0.8
	}
	if confidence > 1 {
		confidence = 1
	}
	evidence := 0
	if opts.Evidence {
		evidence = 1
	}
	var validUntil int64
	if !opts.ValidUntil.IsZero() {
		validUntil = opts.ValidUntil.UnixNano()
	}
	now := time.Now().UnixNano()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO knowledge_facts(archetype, body, confidence, created_at, updated_at, hit_count, revision_count, scope, active, valid_until, evidence)
		   VALUES(?,?,?,?,?,0,0,?,1,?,?)
		   ON CONFLICT(body) DO UPDATE SET
		     archetype=excluded.archetype,
		     confidence=MAX(confidence, excluded.confidence),
		     updated_at=excluded.updated_at,
		     revision_count=revision_count+1,
		     scope=CASE WHEN excluded.scope != '' THEN excluded.scope ELSE scope END,
		     active=1,
		     superseded_by=NULL,
		     valid_until=excluded.valid_until,
		     evidence=excluded.evidence`,
		archetype, body, confidence, now, now, opts.Scope, validUntil, evidence)
	if err != nil {
		return 0, err
	}
	var rev int
	_ = s.db.QueryRowContext(ctx, `SELECT revision_count FROM knowledge_facts WHERE body=?`, body).Scan(&rev)
	return rev, nil
}

// KnowledgeSupersede stores a new fact and atomically marks the given
// supersededID inactive. If the new fact body already exists it is updated
// (re-confirmation). Returns the revision count of the new fact.
// The superseded fact remains on disk for audit but is excluded from all
// recall queries.
func (s *knowledgeStore) KnowledgeSupersede(ctx context.Context, supersededID int64, archetype, body string, confidence float64, opts KnowledgeAddOpts) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck

	// Verify the superseded fact exists and is active.
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT active FROM knowledge_facts WHERE id=?`, supersededID).Scan(&active); err != nil {
		return 0, fmt.Errorf("superseded fact #%d not found: %w", supersededID, err)
	}

	// Add or update the new fact.
	rev, err := s.addInTx(ctx, tx, archetype, body, confidence, opts)
	if err != nil {
		return 0, err
	}

	// Fetch the new fact's id.
	var newID int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM knowledge_facts WHERE body=?`, body).Scan(&newID); err != nil {
		return 0, fmt.Errorf("fetch new fact id: %w", err)
	}

	// Mark the old fact superseded.
	if _, err := tx.ExecContext(ctx,
		`UPDATE knowledge_facts SET active=0, superseded_by=? WHERE id=?`,
		newID, supersededID,
	); err != nil {
		return 0, err
	}

	return rev, tx.Commit()
}

// addInTx is KnowledgeAddFull inside an existing transaction.
func (s *knowledgeStore) addInTx(ctx context.Context, tx *sql.Tx, archetype, body string, confidence float64, opts KnowledgeAddOpts) (int, error) {
	if confidence <= 0 {
		confidence = 0.8
	}
	if confidence > 1 {
		confidence = 1
	}
	evidence := 0
	if opts.Evidence {
		evidence = 1
	}
	var validUntil int64
	if !opts.ValidUntil.IsZero() {
		validUntil = opts.ValidUntil.UnixNano()
	}
	now := time.Now().UnixNano()
	_, err := tx.ExecContext(ctx,
		`INSERT INTO knowledge_facts(archetype, body, confidence, created_at, updated_at, hit_count, revision_count, scope, active, valid_until, evidence)
		   VALUES(?,?,?,?,?,0,0,?,1,?,?)
		   ON CONFLICT(body) DO UPDATE SET
		     archetype=excluded.archetype,
		     confidence=MAX(confidence, excluded.confidence),
		     updated_at=excluded.updated_at,
		     revision_count=revision_count+1,
		     scope=CASE WHEN excluded.scope != '' THEN excluded.scope ELSE scope END,
		     active=1,
		     superseded_by=NULL,
		     valid_until=excluded.valid_until,
		     evidence=excluded.evidence`,
		archetype, body, confidence, now, now, opts.Scope, validUntil, evidence)
	if err != nil {
		return 0, err
	}
	var rev int
	_ = tx.QueryRowContext(ctx, `SELECT revision_count FROM knowledge_facts WHERE body=?`, body).Scan(&rev)
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
	now := time.Now().UnixNano()
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, archetype, body, confidence, created_at, updated_at, hit_count, revision_count, scope
		   FROM knowledge_facts WHERE scope != '' AND `+activeFilter(now))
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
	// Apply the default cap when max=0 (unspecified) to avoid flooding context
	// on broad scopes. Callers that want uncapped results must pass max<0.
	if max == 0 {
		max = 10
	}
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
// confidence × archetype weight (max ≈ 1.6).
func qualityWeight(f KnowledgeFact) float64 {
	return f.Confidence * archetypeWeight(f.Archetype)
}

// activeFilter is the WHERE clause fragment that excludes superseded and
// expired facts. Must be ANDed into every recall query.
// valid_until=0 means no expiry.
func activeFilter(nowNs int64) string {
	return fmt.Sprintf("active=1 AND (valid_until=0 OR valid_until>%d)", nowNs)
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

// KnowledgeQuery returns the top-k active, non-expired facts ordered by salience
// (confidence × archetype weight × recency decay).
// Pass k<=0 for the default (10).
func (s *knowledgeStore) KnowledgeQuery(ctx context.Context, k int) ([]KnowledgeFact, error) {
	k = clampK(k)
	now := time.Now().UnixNano()
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, archetype, body, confidence, created_at, updated_at, hit_count, revision_count
		   FROM knowledge_facts
		   WHERE `+activeFilter(now)+`
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
	now := time.Now().UnixNano()
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, archetype, body, confidence, created_at, updated_at, hit_count, revision_count
		   FROM knowledge_facts WHERE `+activeFilter(now))
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

// KnowledgeExportAll returns every active, non-expired fact, UNCAPPED
// (KnowledgeQuery clamps to 50), newest-confident first. Includes scope so the
// export→import round-trip preserves gotcha-on-touch bindings (#645).
func (s *knowledgeStore) KnowledgeExportAll(ctx context.Context) ([]KnowledgeFact, error) {
	now := time.Now().UnixNano()
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, archetype, body, confidence, created_at, updated_at, hit_count, revision_count, scope
		   FROM knowledge_facts
		   WHERE `+activeFilter(now)+`
		   ORDER BY confidence DESC, updated_at DESC`)
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
		out = append(out, f)
	}
	return out, rows.Err()
}

// KnowledgeBackup is the portable note shape used to rescue notes across a
// reindex — including one triggered BY a schema-version mismatch (#648). It
// round-trips every field that survives a rebuild: the scope binding (#645) and
// the created_at / updated_at / hit_count / revision_count that feed Salience.
// Dropping these on restore silently unscopes notes and resets their ranking
// signal (#678).
// v6 additions: evidence (#618) and valid_until (#618) are preserved.
// superseded_by is NOT exported — the FK is id-based and would be meaningless
// after the row-ids change on a reindex. Only active facts are exported.
type KnowledgeBackup struct {
	Archetype     string
	Body          string
	Confidence    float64
	Scope         string
	CreatedAt     int64 // unix nanos; 0 when the source schema predates the column
	UpdatedAt     int64 // unix nanos; 0 when the source schema predates the column
	HitCount      int
	RevisionCount int
	Pinned        bool  // #633; false when the source schema predates the column
	Evidence      bool  // #618; false when the source schema predates the column
	ValidUntil    int64 // #618; 0 = no expiry or when predates the column
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
	// Build a fixed 9-column projection: present columns select directly, absent
	// ones fall back to a literal default so the scan signature never changes.
	col := func(name, def string) string {
		if cols[name] {
			return name
		}
		return def + " AS " + name
	}
	// Only rescue active facts — inactive (superseded) ones are intentionally
	// soft-deleted and should not be restored.
	activeExpr := ""
	if cols["active"] {
		activeExpr = " WHERE active=1"
	}
	query := `SELECT archetype, body, confidence, ` +
		col("scope", "''") + `, ` +
		col("created_at", "0") + `, ` +
		col("updated_at", "0") + `, ` +
		col("hit_count", "0") + `, ` +
		col("revision_count", "0") + `, ` +
		col("pinned", "0") + `, ` +
		col("evidence", "0") + `, ` +
		col("valid_until", "0") +
		` FROM knowledge_facts` + activeExpr + ` ORDER BY id`
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, nil // file/table missing or unreadable → nothing to rescue
	}
	defer func() { _ = rows.Close() }()
	var out []KnowledgeBackup
	for rows.Next() {
		var b KnowledgeBackup
		var pinned, evidence int
		if err := rows.Scan(&b.Archetype, &b.Body, &b.Confidence, &b.Scope,
			&b.CreatedAt, &b.UpdatedAt, &b.HitCount, &b.RevisionCount, &pinned, &evidence, &b.ValidUntil); err != nil {
			return nil, err
		}
		b.Pinned = pinned != 0
		b.Evidence = evidence != 0
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
	evidence := 0
	if b.Evidence {
		evidence = 1
	}
	pinned := 0
	if b.Pinned {
		pinned = 1
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO knowledge_facts(archetype, body, confidence, created_at, updated_at, hit_count, revision_count, scope, pinned, evidence, valid_until)
		   VALUES(?,?,?,?,?,?,?,?,?,?,?)
		   ON CONFLICT(body) DO UPDATE SET
		     archetype=excluded.archetype,
		     confidence=MAX(confidence, excluded.confidence),
		     updated_at=MAX(updated_at, excluded.updated_at),
		     hit_count=MAX(hit_count, excluded.hit_count),
		     revision_count=MAX(revision_count, excluded.revision_count),
		     scope=CASE WHEN excluded.scope != '' THEN excluded.scope ELSE scope END,
		     pinned=CASE WHEN excluded.pinned=1 THEN 1 ELSE pinned END,
		     evidence=CASE WHEN excluded.evidence=1 THEN 1 ELSE evidence END,
		     valid_until=CASE WHEN excluded.valid_until!=0 THEN excluded.valid_until ELSE valid_until END`,
		b.Archetype, b.Body, b.Confidence, b.CreatedAt, b.UpdatedAt, b.HitCount, b.RevisionCount, b.Scope, pinned, evidence, b.ValidUntil)
	return err
}

// KnowledgeCount returns the number of stored facts.
func (s *knowledgeStore) KnowledgeCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM knowledge_facts`).Scan(&n)
	return n, err
}

// KnowledgeDelete removes a fact by id, pruning any knowledge_relations that
// reference it first (SQLite FK cascade is not available without a schema
// rebuild, so we do it explicitly — #756).
func (s *knowledgeStore) KnowledgeDelete(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM knowledge_relations WHERE from_id=? OR to_id=?`, id, id); err != nil {
		return fmt.Errorf("delete relations for fact %d: %w", id, err)
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM knowledge_facts WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("fact not found")
	}
	return nil
}

// KnowledgeByID returns a single fact by its primary key.
// Returns (zero, errors.New("fact not found")) when no row exists.
func (s *knowledgeStore) KnowledgeByID(ctx context.Context, id int64) (KnowledgeFact, error) {
	var f KnowledgeFact
	var cNs, uNs, vuNs int64
	var vuValid bool
	err := s.db.QueryRowContext(ctx,
		`SELECT id, archetype, body, confidence, created_at, updated_at,
		        hit_count, revision_count, scope, pinned, active, superseded_by,
		        evidence, CASE WHEN valid_until IS NULL THEN 0 ELSE valid_until END, valid_until IS NOT NULL
		 FROM knowledge_facts WHERE id=?`, id,
	).Scan(&f.ID, &f.Archetype, &f.Body, &f.Confidence, &cNs, &uNs,
		&f.HitCount, &f.RevisionCount, &f.Scope, &f.Pinned, &f.Active, &f.SupersededBy,
		&f.Evidence, &vuNs, &vuValid)
	if errors.Is(err, sql.ErrNoRows) {
		return KnowledgeFact{}, errors.New("fact not found")
	}
	if err != nil {
		return KnowledgeFact{}, err
	}
	f.CreatedAt = time.Unix(0, cNs)
	f.UpdatedAt = time.Unix(0, uNs)
	if vuValid {
		f.ValidUntil = time.Unix(0, vuNs)
	}
	f.Salience = qualityWeight(f) * recencyFactor(f.UpdatedAt)
	return f, nil
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
	nowNs := time.Now().UnixNano()
	q := `SELECT id, archetype, body, confidence, created_at, updated_at, hit_count, revision_count
	        FROM knowledge_facts f
	       WHERE ` + activeFilter(nowNs) + ` AND NOT EXISTS (SELECT 1 FROM fact_vecs v WHERE v.rowid = f.id)
	       ORDER BY updated_at DESC
	       LIMIT ?`
	rows, err := s.db.QueryContext(ctx, q, limit)
	if err != nil {
		// fact_vecs missing → treat all facts as missing.
		rows, err = s.db.QueryContext(ctx,
			`SELECT id, archetype, body, confidence, created_at, updated_at, hit_count, revision_count
			   FROM knowledge_facts WHERE `+activeFilter(nowNs)+` ORDER BY updated_at DESC LIMIT ?`, limit)
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
	// strict mode: skip-fallback callers (locate/review) get nil when no
	// vector search is possible so the caller decides — not a silent salience
	// result that bypasses their noFallback guard (#706).
	strict := minSim > 0
	if len(queryVec) == 0 {
		if strict {
			return nil, nil
		}
		return s.KnowledgeQuery(ctx, k)
	}
	var cnt int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM fact_vecs`).Scan(&cnt); err != nil || cnt == 0 {
		if strict {
			return nil, nil // table absent or empty; let caller decide (no salience leak)
		}
		return s.KnowledgeQuery(ctx, k)
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
		if strict {
			return nil, nil
		}
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

	nowNs := time.Now().UnixNano()
	factQ := `SELECT id, archetype, body, confidence, created_at, updated_at, hit_count, revision_count FROM knowledge_facts WHERE ` + activeFilter(nowNs) + ` AND id IN (` + inPlaceholders(len(ids)) + `)` //nolint:gosec // placeholder count is generated; ids passed as bind args
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
	JaccardMerge    float64 // word-overlap ≥ this AUTO-merges two facts of the same archetype (default 0.95, #633)
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
		// #633: only near-identical bodies auto-merge unattended. The looser
		// 0.85–0.95 band is surfaced as a `notes review` proposal for the
		// agent to judge (supersede / keep-both / relate) — never auto-deleted.
		c.JaccardMerge = 0.95
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
// Pinned facts (#633) are exempt — never selected, never decayed.
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

	// VerifiedFact archetype is decay-exempt (like pinned), because the author
	// explicitly declared it verified. It decays only via explicit supersession.
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, archetype, confidence, hit_count, last_retrieved, evidence
		   FROM knowledge_facts WHERE pinned=0 AND active=1 AND archetype!='VerifiedFact'`)
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
		var archetype string
		var conf float64
		var hitCount int
		var lastRetrieved int64
		var evidence int
		if err := rows.Scan(&id, &archetype, &conf, &hitCount, &lastRetrieved, &evidence); err != nil {
			_ = rows.Close()
			return 0, err
		}
		rate := archetypeDecayRate(archetype)
		if evidence != 0 {
			rate *= 0.5 // code-inspection facts decay at half speed
		}
		protect := 1 + math.Log(1+float64(hitCount))
		decay := rate * elapsedDays / protect
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
// word-sets overlap at or above cfg.JaccardMerge (default 0.95 — only near-
// identical bodies). The higher-confidence fact survives, accumulating the
// other's hit_count and scope binding; the duplicate is deleted (its fact_vecs
// row cascades away via trigger). A pinned duplicate (#633) is never deleted —
// the author declared it permanent, so it is skipped as a merge target.
func (s *knowledgeStore) knowledgeConsolidateSimilar(ctx context.Context, cfg KnowledgeGCConfig) (int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, archetype, body, confidence, hit_count, scope, pinned FROM knowledge_facts WHERE active=1 ORDER BY archetype, confidence DESC`)
	if err != nil {
		return 0, err
	}
	type fact struct {
		id        int64
		archetype string
		body      string
		conf      float64
		hits      int
		scope     string
		pinned    bool
		words     map[string]struct{}
	}
	var facts []fact
	for rows.Next() {
		var f fact
		if err := rows.Scan(&f.id, &f.archetype, &f.body, &f.conf, &f.hits, &f.scope, &f.pinned); err != nil {
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
			if facts[j].pinned {
				continue // pinned duplicate is permanent — never auto-deleted
			}
			if jaccard(facts[i].words, facts[j].words) < cfg.JaccardMerge {
				continue
			}
			// facts are confidence-desc within archetype, so i is always the keeper.
			keeper, dup := &facts[i], &facts[j]
			// Merge hit_count, confidence, and scope: if the keeper has no scope
			// but the duplicate does, adopt the duplicate's scope so the binding
			// is not silently lost when the duplicate is deleted.
			if _, err := s.db.ExecContext(ctx,
				`UPDATE knowledge_facts SET hit_count=hit_count+?, confidence=MAX(confidence, ?),
				   scope=CASE WHEN scope='' AND ?!='' THEN ? ELSE scope END WHERE id=?`,
				dup.hits, dup.conf, dup.scope, dup.scope, keeper.id); err != nil {
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
// configured cap. Ties break toward the least-recently-retrieved. Pinned facts
// (#633) are never evicted — they still count toward the cap, but the cap is a
// soft ceiling, so a store full of pinned facts simply stops shrinking.
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
		   WHERE pinned=0 AND active=1
		   ORDER BY confidence ASC, last_retrieved ASC, updated_at ASC
		   LIMIT ?)`, excess)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// KnowledgeSetPinned sets or clears the pinned flag on a fact (#633). A pinned
// fact is exempt from confidence decay, cap eviction, and `notes review`
// staleness proposals. Returns an error if no fact has the given id.
func (s *knowledgeStore) KnowledgeSetPinned(ctx context.Context, id int64, pinned bool) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE knowledge_facts SET pinned=? WHERE id=?`, pinned, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no fact with id %d", id)
	}
	return nil
}

// Review thresholds (#633). Advisory only — KnowledgeReview never mutates; the
// agent reads the proposals and decides. The merge band starts below the GC
// auto-merge threshold (0.95) so the 0.85–0.95 overlap that GC no longer
// auto-deletes surfaces here for the agent to judge.
const (
	reviewMergeJaccard   = 0.85 // ≥ this AND same archetype → propose merge/supersede
	reviewOverlapJaccard = 0.5  // ≥ this (any archetype) → propose overlap review
	reviewStaleDays      = 30   // unpinned, zero-hit, older than this → propose stale
)

// ReviewProposal is one advisory cleanup suggestion. It never mutates the store.
type ReviewProposal struct {
	Kind       string   `json:"kind"`                 // "merge" | "overlap" | "stale"
	IDs        []int64  `json:"ids"`                  // fact id(s) involved
	Bodies     []string `json:"bodies"`               // their bodies, for the agent to judge
	Similarity float64  `json:"similarity,omitempty"` // word-overlap, for merge/overlap
	Reason     string   `json:"reason"`               // one-line hint
}

// KnowledgeReviewResult groups proposals by kind. Total is the proposal count,
// consumed by the session-start nudge.
type KnowledgeReviewResult struct {
	Merge   []ReviewProposal `json:"merge"`
	Overlap []ReviewProposal `json:"overlap"`
	Stale   []ReviewProposal `json:"stale"`
	Total   int              `json:"total"`
}

// KnowledgeReview computes advisory cleanup proposals over the fact store
// WITHOUT mutating anything (#633): near-duplicate merges, looser overlaps for
// the agent to judge (supersede / keep-both / relate), and stale unpinned
// facts. It is the read-only companion to KnowledgeGC — the agent acts on the
// output; dex never auto-mutates on these heuristics. O(n²) over a ≤1000-row
// store, comfortably sub-millisecond.
func (s *knowledgeStore) KnowledgeReview(ctx context.Context) (KnowledgeReviewResult, error) {
	var res KnowledgeReviewResult
	nowNs := time.Now().UnixNano()
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, archetype, body, updated_at, hit_count, scope, pinned FROM knowledge_facts WHERE `+activeFilter(nowNs))
	if err != nil {
		return res, err
	}
	type rfact struct {
		id        int64
		archetype string
		body      string
		updatedAt int64
		hits      int
		scope     string
		pinned    bool
		words     map[string]struct{}
	}
	var facts []rfact
	for rows.Next() {
		var f rfact
		if err := rows.Scan(&f.id, &f.archetype, &f.body, &f.updatedAt, &f.hits, &f.scope, &f.pinned); err != nil {
			_ = rows.Close()
			return res, err
		}
		f.words = wordSet(f.body)
		facts = append(facts, f)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return res, err
	}
	_ = rows.Close()

	for i := range facts {
		for j := i + 1; j < len(facts); j++ {
			a, b := facts[i], facts[j]
			sim := jaccard(a.words, b.words)
			switch {
			case a.archetype == b.archetype && sim >= reviewMergeJaccard:
				res.Merge = append(res.Merge, ReviewProposal{
					Kind: "merge", IDs: []int64{a.id, b.id},
					Bodies: []string{a.body, b.body}, Similarity: sim,
					Reason: fmt.Sprintf("near-duplicate %s facts (overlap %.2f) — merge or supersede", a.archetype, sim),
				})
			case sim >= reviewOverlapJaccard:
				res.Overlap = append(res.Overlap, ReviewProposal{
					Kind: "overlap", IDs: []int64{a.id, b.id},
					Bodies: []string{a.body, b.body}, Similarity: sim,
					Reason: fmt.Sprintf("overlapping facts (overlap %.2f) — supersede, keep both, or relate", sim),
				})
			case a.scope != "" && a.scope == b.scope:
				res.Overlap = append(res.Overlap, ReviewProposal{
					Kind: "overlap", IDs: []int64{a.id, b.id},
					Bodies: []string{a.body, b.body},
					Reason: fmt.Sprintf("two facts share scope %q — check for redundancy", a.scope),
				})
			}
		}
	}

	cutoff := time.Now().Add(-reviewStaleDays * 24 * time.Hour).UnixNano()
	for _, f := range facts {
		if f.pinned || f.hits > 0 || f.updatedAt == 0 || f.updatedAt > cutoff {
			continue
		}
		ageDays := int(time.Since(time.Unix(0, f.updatedAt)).Hours() / 24)
		res.Stale = append(res.Stale, ReviewProposal{
			Kind: "stale", IDs: []int64{f.id}, Bodies: []string{f.body},
			Reason: fmt.Sprintf("%dd old, never retrieved — evict, rewrite, or pin", ageDays),
		})
	}

	res.Total = len(res.Merge) + len(res.Overlap) + len(res.Stale)
	return res, nil
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
