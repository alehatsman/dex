package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Agent is a registered participant in the coordination bus.
type Agent struct {
	ID          string
	Role        string
	AnnouncedAt time.Time
	LastSeenAt  time.Time
}

// AgentMessage is one message posted to the coordination bus.
type AgentMessage struct {
	ID       int64
	AgentID  string
	Role     string
	Topic    string
	Category string
	Body     string
	PostedAt time.Time
}

// AgentAnnounce registers or refreshes an agent. Upserts on id.
func (s *Store) AgentAnnounce(ctx context.Context, agentID, role string) error {
	now := time.Now().UnixNano()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO agents(id, role, announced_at, last_seen_at) VALUES(?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET role=excluded.role, last_seen_at=excluded.last_seen_at`,
		agentID, role, now, now)
	return err
}

// AgentList returns all registered agents ordered by last_seen_at descending.
func (s *Store) AgentList(ctx context.Context) ([]Agent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, role, announced_at, last_seen_at FROM agents ORDER BY last_seen_at DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Agent
	for rows.Next() {
		var a Agent
		var annNs, seenNs int64
		if err := rows.Scan(&a.ID, &a.Role, &annNs, &seenNs); err != nil {
			return nil, err
		}
		a.AnnouncedAt = time.Unix(0, annNs)
		a.LastSeenAt = time.Unix(0, seenNs)
		out = append(out, a)
	}
	return out, rows.Err()
}

// AgentPost appends a message to the bus and bumps the poster's last_seen_at.
// category is an optional semantic label (e.g. "finding", "plan", "error").
// Returns the new message id.
func (s *Store) AgentPost(ctx context.Context, agentID, topic, category, body string) (int64, error) {
	now := time.Now().UnixNano()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO agent_messages(agent_id, topic, category, body, posted_at) VALUES(?,?,?,?,?)`,
		agentID, topic, category, body, now)
	if err != nil {
		return 0, err
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE agents SET last_seen_at=? WHERE id=?`, now, agentID)
	return res.LastInsertId()
}

// AgentRead returns messages from the bus, optionally filtered by topic,
// category, or full-text query. sinceID is an exclusive lower bound on message
// id for pagination. limit defaults to 50.
func (s *Store) AgentRead(ctx context.Context, topic, category, query string, sinceID int64, limit int) ([]AgentMessage, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	const sel = `SELECT m.id, m.agent_id, COALESCE(a.role,''), m.topic, COALESCE(m.category,''), m.body, m.posted_at
	              FROM agent_messages m LEFT JOIN agents a ON a.id=m.agent_id`

	// FTS path: join against agent_messages_fts when a text query is given.
	if query != "" {
		ftsQ := buildAgentFTSQuery(query)
		q := sel + ` INNER JOIN agent_messages_fts f ON f.rowid=m.id
		             WHERE agent_messages_fts MATCH ?`
		args := []any{ftsQ}
		if topic != "" {
			q += " AND m.topic=?"
			args = append(args, topic)
		}
		if category != "" {
			q += " AND m.category=?"
			args = append(args, category)
		}
		q += fmt.Sprintf(" AND m.id>? ORDER BY rank LIMIT %d", limit)
		args = append(args, sinceID)
		rows, err := s.db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, err
		}
		return scanAgentMessages(rows)
	}

	// Non-FTS path: filter by topic/category/sinceID.
	var conds []string
	var args []any
	conds = append(conds, "m.id>?")
	args = append(args, sinceID)
	if topic != "" {
		conds = append(conds, "m.topic=?")
		args = append(args, topic)
	}
	if category != "" {
		conds = append(conds, "m.category=?")
		args = append(args, category)
	}
	q := sel + " WHERE " + strings.Join(conds, " AND ") + fmt.Sprintf(" ORDER BY m.id ASC LIMIT %d", limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return scanAgentMessages(rows)
}

func scanAgentMessages(rows *sql.Rows) ([]AgentMessage, error) {
	defer func() { _ = rows.Close() }()
	var out []AgentMessage
	for rows.Next() {
		var m AgentMessage
		var tsNs int64
		if err := rows.Scan(&m.ID, &m.AgentID, &m.Role, &m.Topic, &m.Category, &m.Body, &tsNs); err != nil {
			return nil, err
		}
		m.PostedAt = time.Unix(0, tsNs)
		out = append(out, m)
	}
	return out, rows.Err()
}

// buildAgentFTSQuery wraps each whitespace-delimited word in double quotes for
// FTS5 exact-token matching. Short single-word queries are passed through directly.
func buildAgentFTSQuery(q string) string {
	words := strings.Fields(q)
	if len(words) == 0 {
		return q
	}
	quoted := make([]string, 0, len(words))
	for _, w := range words {
		w = strings.Trim(w, `"'`)
		if w != "" {
			quoted = append(quoted, `"`+strings.ReplaceAll(w, `"`, "")+`"`)
		}
	}
	return strings.Join(quoted, " ")
}

// buildAgentFTSQueryOr joins the query words with FTS5 OR instead of the
// implicit AND of buildAgentFTSQuery. This is the DEX_EMBED_ENGINE=none
// fallback recall path for the findings bus: a terse finding ("assemble
// overflows the tool-result cap") must still surface for a natural-language
// question ("why did the pack get truncated?") when no embedder is available to
// do real vector recall. AND-matching every word never recalls such a finding
// (#180; the #168 spike proved the gap). Vector recall (AgentQueryVec) is the
// primary path — this is only reached when the embedder is absent.
func buildAgentFTSQueryOr(q string) string {
	words := strings.Fields(q)
	if len(words) == 0 {
		return q
	}
	quoted := make([]string, 0, len(words))
	for _, w := range words {
		w = strings.Trim(w, `"'`)
		if w != "" {
			quoted = append(quoted, `"`+strings.ReplaceAll(w, `"`, "")+`"`)
		}
	}
	return strings.Join(quoted, " OR ")
}

// AgentReadAny mirrors AgentRead but matches ANY query word (FTS5 OR) rather
// than all of them. It is the no-embedder fallback for findings-bus recall; see
// buildAgentFTSQueryOr. With an empty query it is identical to AgentRead.
func (s *Store) AgentReadAny(ctx context.Context, topic, category, query string, sinceID int64, limit int) ([]AgentMessage, error) {
	if query == "" {
		return s.AgentRead(ctx, topic, category, query, sinceID, limit)
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	const sel = `SELECT m.id, m.agent_id, COALESCE(a.role,''), m.topic, COALESCE(m.category,''), m.body, m.posted_at
	              FROM agent_messages m LEFT JOIN agents a ON a.id=m.agent_id`
	q := sel + ` INNER JOIN agent_messages_fts f ON f.rowid=m.id
	             WHERE agent_messages_fts MATCH ?`
	args := []any{buildAgentFTSQueryOr(query)}
	if topic != "" {
		q += " AND m.topic=?"
		args = append(args, topic)
	}
	if category != "" {
		q += " AND m.category=?"
		args = append(args, category)
	}
	q += fmt.Sprintf(" AND m.id>? ORDER BY rank LIMIT %d", limit)
	args = append(args, sinceID)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return scanAgentMessages(rows)
}

// ─── semantic recall (vec0-backed) ──────────────────────────────────────────
//
// The findings bus recalls peer messages by vector similarity, mirroring the
// knowledge_facts fact_vecs machinery (store_knowledge.go). Bus messages tagged
// category=finding are embedded on post through the same embedder as facts; a
// natural-language question then recalls a terse peer finding that FTS
// implicit-AND never would (the #168 spike's load-bearing gap). Vectors live in
// a sidecar vec0 table keyed on the agent_messages rowid — the same shape as
// fact_vecs, not a column on the row — so KNN MATCH/distance comes for free and
// a deleted message drops its vector via an AFTER DELETE trigger.

// ensureAgentVecTable materializes the agent_msg_vecs vec0 virtual table at the
// given embedding dimension plus the delete-cascade trigger. Idempotent; on a
// dimension change (embed model swapped) it drops and recreates, and vectors
// re-backfill lazily on the next post. Mirrors ensureFactVecTable.
func (s *Store) ensureAgentVecTable(ctx context.Context, dim int) error {
	return ensureSidecarVecTable(ctx, s.db, sidecarVecSpec{
		vecTable:   "agent_msg_vecs",
		srcTable:   "agent_messages",
		trigger:    "agent_messages_vec_ad",
		dimMetaKey: "agent_msg_vecs_dim",
	}, dim)
}

// AgentUpsertVec stores (or replaces) the embedding for a bus message id. The
// vec0 table is created on first use at the vector's dimension. Empty vec is a
// no-op. Mirrors KnowledgeUpsertVec.
func (s *Store) AgentUpsertVec(ctx context.Context, id int64, vec []float32) error {
	if len(vec) == 0 {
		return nil
	}
	if err := s.ensureAgentVecTable(ctx, len(vec)); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM agent_msg_vecs WHERE rowid=?`, id); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO agent_msg_vecs(rowid, embedding) VALUES(?, ?)`, id, encodeVec(vec))
	return err
}

// AgentPostVec posts a message and, when vec is non-empty, embeds it in one
// step — the write path for category=finding. Returns the new message id. It
// also opportunistically prunes messages past the retention window so the bus
// bounds its own growth (the pragmatic-hybrid lifecycle: findings age out, they
// don't accrete — #180); a finding that stayed useful has by then been promoted
// to a durable fact via the normal remember/supersede path.
func (s *Store) AgentPostVec(ctx context.Context, agentID, topic, category, body string, vec []float32) (int64, error) {
	id, err := s.AgentPost(ctx, agentID, topic, category, body)
	if err != nil {
		return 0, err
	}
	if err := s.AgentUpsertVec(ctx, id, vec); err != nil {
		return id, err
	}
	_, _ = s.AgentPrune(ctx, time.Now().Add(-agentMsgRetention)) // best-effort GC
	return id, nil
}

// agentMsgRetention is how long a bus message lingers in the store before
// write-time GC removes it. Deliberately wider than the fold TTL
// (peerFindingTTL, 24h) so an aged-out finding is still queryable via
// `dex agent read` for a while before it's deleted for good.
const agentMsgRetention = 7 * 24 * time.Hour

// AgentPrune deletes bus messages posted before the cutoff and returns the
// number removed. The AFTER DELETE trigger (agent_messages_vec_ad) cascades to
// agent_msg_vecs, so no vector is orphaned.
func (s *Store) AgentPrune(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM agent_messages WHERE posted_at < ?`, before.UnixNano())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// AgentQueryVec returns up to k bus messages ranked by cosine similarity to
// queryVec, filtered to category (empty = any) and to a similarity floor minSim.
// Unlike KnowledgeQueryVec there is no quality/recency hybrid — bus messages are
// ephemeral peer signals with no confidence or archetype; recency/TTL and
// self-filtering are policy applied by the caller. Returns nil (not an error,
// not a fallback) when queryVec is empty or no message vectors exist, so the
// caller can decide whether to try the FTS-OR fallback.
func (s *Store) AgentQueryVec(ctx context.Context, queryVec []float32, category string, k int, minSim float64) ([]AgentMessage, error) {
	if k <= 0 {
		k = 10
	}
	if len(queryVec) == 0 {
		return nil, nil
	}
	var cnt int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_msg_vecs`).Scan(&cnt); err != nil || cnt == 0 {
		return nil, nil // table absent or empty
	}
	pool := k * 4
	if pool < 20 {
		pool = 20
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT rowid, distance FROM agent_msg_vecs
		 WHERE embedding MATCH ? AND k = ?
		 ORDER BY distance`, encodeVec(queryVec), pool)
	if err != nil {
		return nil, nil
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
		sim := 1 - dist
		if sim < minSim {
			continue
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
		return nil, nil
	}

	sel := `SELECT m.id, m.agent_id, COALESCE(a.role,''), m.topic, COALESCE(m.category,''), m.body, m.posted_at
	          FROM agent_messages m LEFT JOIN agents a ON a.id=m.agent_id
	         WHERE m.id IN (` + inPlaceholders(len(ids)) + `)` //nolint:gosec // placeholder count generated; ids bound as args
	args := int64sToAny(ids)
	if category != "" {
		sel += " AND m.category=?"
		args = append(args, category)
	}
	mrows, err := s.db.QueryContext(ctx, sel, args...)
	if err != nil {
		return nil, err
	}
	msgs, err := scanAgentMessages(mrows)
	if err != nil {
		return nil, err
	}
	// Order by similarity descending (the vec fetch order is lost by the IN join).
	sort.SliceStable(msgs, func(i, j int) bool { return sims[msgs[i].ID] > sims[msgs[j].ID] })
	if len(msgs) > k {
		msgs = msgs[:k]
	}
	return msgs, nil
}
