package store

import (
	"context"
	"database/sql"
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
	defer rows.Close()
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
// Returns the new message id.
func (s *Store) AgentPost(ctx context.Context, agentID, topic, body string) (int64, error) {
	now := time.Now().UnixNano()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO agent_messages(agent_id, topic, body, posted_at) VALUES(?,?,?,?)`,
		agentID, topic, body, now)
	if err != nil {
		return 0, err
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE agents SET last_seen_at=? WHERE id=?`, now, agentID)
	return res.LastInsertId()
}

// AgentRead returns messages from the bus, optionally filtered by topic and
// paginated with sinceID (exclusive lower bound on message id). limit defaults to 50.
func (s *Store) AgentRead(ctx context.Context, topic string, sinceID int64, limit int) ([]AgentMessage, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	const base = `SELECT m.id, m.agent_id, COALESCE(a.role,''), m.topic, m.body, m.posted_at
	                FROM agent_messages m LEFT JOIN agents a ON a.id=m.agent_id`
	var (
		rows *sql.Rows
		err  error
	)
	if topic != "" {
		rows, err = s.db.QueryContext(ctx, base+
			` WHERE m.topic=? AND m.id>? ORDER BY m.id ASC LIMIT ?`,
			topic, sinceID, limit)
	} else {
		rows, err = s.db.QueryContext(ctx, base+
			` WHERE m.id>? ORDER BY m.id ASC LIMIT ?`,
			sinceID, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentMessage
	for rows.Next() {
		var m AgentMessage
		var tsNs int64
		if err := rows.Scan(&m.ID, &m.AgentID, &m.Role, &m.Topic, &m.Body, &tsNs); err != nil {
			return nil, err
		}
		m.PostedAt = time.Unix(0, tsNs)
		out = append(out, m)
	}
	return out, rows.Err()
}
