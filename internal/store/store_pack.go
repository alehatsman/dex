package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ErrPackNotFound is returned when a package name is not registered.
var ErrPackNotFound = errors.New("package not found")

// CtxPackRecord tracks a registered context package in the project index.
type CtxPackRecord struct {
	Name      string
	CreatedAt time.Time
	AutoLoad  bool
}

// PackRegister inserts or updates a package record.
func (s *Store) PackRegister(ctx context.Context, name string, autoLoad bool) error {
	now := time.Now().UnixNano()
	al := 0
	if autoLoad {
		al = 1
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO ctx_packages(name, created_at, auto_load) VALUES(?,?,?)
		 ON CONFLICT(name) DO UPDATE SET created_at=excluded.created_at, auto_load=excluded.auto_load`,
		name, now, al)
	return err
}

// PackSetAutoLoad marks a package for automatic loading on ctx_overview.
func (s *Store) PackSetAutoLoad(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE ctx_packages SET auto_load=1 WHERE name=?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrPackNotFound
	}
	return nil
}

// PackList returns all registered packages.
func (s *Store) PackList(ctx context.Context) ([]CtxPackRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, created_at, auto_load FROM ctx_packages ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []CtxPackRecord
	for rows.Next() {
		var r CtxPackRecord
		var tsNs int64
		var al int
		if err := rows.Scan(&r.Name, &tsNs, &al); err != nil {
			return nil, err
		}
		r.CreatedAt = time.Unix(0, tsNs)
		r.AutoLoad = al != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// PackGet returns one registered package record by name.
func (s *Store) PackGet(ctx context.Context, name string) (CtxPackRecord, bool, error) {
	var r CtxPackRecord
	var tsNs int64
	var al int
	err := s.db.QueryRowContext(ctx,
		`SELECT name, created_at, auto_load FROM ctx_packages WHERE name=?`, name).
		Scan(&r.Name, &tsNs, &al)
	if err == sql.ErrNoRows {
		return r, false, nil
	}
	if err != nil {
		return r, false, err
	}
	r.CreatedAt = time.Unix(0, tsNs)
	r.AutoLoad = al != 0
	return r, true, nil
}
