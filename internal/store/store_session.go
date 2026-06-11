package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// SessionState is the persisted session for one project.
type SessionState struct {
	ID        int64
	StartedAt time.Time
	UpdatedAt time.Time
	Task      string
	Notes     string // newline-separated note entries
	Files     []SessionFile
}

// SessionFile is one file access recorded during a session.
type SessionFile struct {
	Path      string
	Op        string // "read" | "write"
	TouchedAt time.Time
}

// SessionGet returns the current (most-recent) session for the project.
// Returns (zero, false, nil) when no session exists yet.
func (s *Store) SessionGet(ctx context.Context) (SessionState, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, started_at, updated_at, task, notes
		   FROM sessions ORDER BY id DESC LIMIT 1`)
	var ss SessionState
	var startNs, updNs int64
	err := row.Scan(&ss.ID, &startNs, &updNs, &ss.Task, &ss.Notes)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return SessionState{}, false, nil
	case err != nil:
		return SessionState{}, false, err
	}
	ss.StartedAt = time.Unix(0, startNs)
	ss.UpdatedAt = time.Unix(0, updNs)

	rows, err := s.db.QueryContext(ctx,
		`SELECT path, op, touched_at FROM session_files
		  WHERE session_id=? ORDER BY touched_at DESC LIMIT 50`,
		ss.ID)
	if err != nil {
		return ss, true, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var f SessionFile
		var tsNs int64
		if err := rows.Scan(&f.Path, &f.Op, &tsNs); err != nil {
			return ss, true, err
		}
		f.TouchedAt = time.Unix(0, tsNs)
		ss.Files = append(ss.Files, f)
	}
	return ss, true, rows.Err()
}

// ensureSession returns the id of the current session, creating one if none exists.
func (s *Store) ensureSession(ctx context.Context) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM sessions ORDER BY id DESC LIMIT 1`).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	now := time.Now().UnixNano()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions(started_at, updated_at, task, notes) VALUES(?,?,?,?)`,
		now, now, "", "")
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// SessionSetTask sets the current task description, creating a session if needed.
func (s *Store) SessionSetTask(ctx context.Context, task string) error {
	id, err := s.ensureSession(ctx)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE sessions SET task=?, updated_at=? WHERE id=?`,
		task, time.Now().UnixNano(), id)
	return err
}

// SessionAddNote appends a timestamped note to the current session.
func (s *Store) SessionAddNote(ctx context.Context, note string) error {
	id, err := s.ensureSession(ctx)
	if err != nil {
		return err
	}
	var existing string
	_ = s.db.QueryRowContext(ctx, `SELECT notes FROM sessions WHERE id=?`, id).Scan(&existing)
	ts := time.Now().Format("2006-01-02 15:04:05")
	var updated string
	if strings.TrimSpace(existing) == "" {
		updated = "[" + ts + "] " + note
	} else {
		updated = existing + "\n[" + ts + "] " + note
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE sessions SET notes=?, updated_at=? WHERE id=?`,
		updated, time.Now().UnixNano(), id)
	return err
}

// SessionAddFile records that a file was accessed in the current session.
func (s *Store) SessionAddFile(ctx context.Context, path, op string) error {
	id, err := s.ensureSession(ctx)
	if err != nil {
		return err
	}
	if _, err = s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO session_files(session_id, path, op, touched_at)
		   VALUES(?,?,?,?)`,
		id, path, op, time.Now().UnixNano()); err != nil {
		return err
	}
	_ = s.RecordCoAccess(ctx, path)
	return nil
}

// SessionTrackFile records a file access only when a task-bearing session
// already exists. Unlike SessionAddFile it never creates a new session.
func (s *Store) SessionTrackFile(ctx context.Context, path, op string) error {
	var id int64
	var task string
	err := s.db.QueryRowContext(ctx, `SELECT id, task FROM sessions ORDER BY id DESC LIMIT 1`).Scan(&id, &task)
	if err != nil || task == "" {
		return nil // no active session with a task — skip silently
	}
	if _, err = s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO session_files(session_id, path, op, touched_at)
		   VALUES(?,?,?,?)`,
		id, path, op, time.Now().UnixNano()); err != nil {
		return err
	}
	_ = s.RecordCoAccess(ctx, path)
	return nil
}

// SessionClear deletes the current session (files cascade via FK).
func (s *Store) SessionClear(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id=(SELECT MAX(id) FROM sessions)`)
	return err
}
