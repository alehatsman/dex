package store

import (
	"context"
	"database/sql"
	"fmt"
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
