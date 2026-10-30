package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// ErrExposeNotFound is returned for a sandbox port that is not exposed.
var ErrExposeNotFound = errors.New("not exposed")

// Expose is a sandbox's port that the head serves at URL, from its own
// loopback port Port. TokenID is the ID of the expose token made for it,
// "" when it is public.
//
// shortcut: exposes live only on the head that made them and are not
// replicated with raft, like access tokens; add both to PoolState together.
type Expose struct {
	Port        int
	Sandbox     string
	SandboxPort int
	Public      bool
	TokenID     string
	URL         string
	CreatedAt   time.Time
}

// CreateExpose stores e, setting CreatedAt.
func (s *Store) CreateExpose(ctx context.Context, e Expose) (Expose, error) {
	e.CreatedAt = time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `INSERT INTO exposes (port, sandbox, sandbox_port, public, token_id, url, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		e.Port, e.Sandbox, e.SandboxPort, e.Public, e.TokenID, e.URL, e.CreatedAt.Format(timeFormat))
	var serr *sqlite.Error
	if errors.As(err, &serr) && serr.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE {
		return Expose{}, fmt.Errorf("port %d of sandbox %q is already exposed", e.SandboxPort, e.Sandbox)
	}
	return e, err
}

const selectExpose = `SELECT port, sandbox, sandbox_port, public, token_id, url, created_at FROM exposes`

// GetExpose returns the expose of port in the named sandbox.
func (s *Store) GetExpose(ctx context.Context, sandbox string, port int) (Expose, error) {
	return scanExpose(s.db.QueryRowContext(ctx, selectExpose+` WHERE sandbox = ? AND sandbox_port = ?`, sandbox, port))
}

// ListExposes returns every expose, oldest first.
func (s *Store) ListExposes(ctx context.Context) ([]Expose, error) {
	rows, err := s.db.QueryContext(ctx, selectExpose+` ORDER BY created_at, port`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []Expose
	for rows.Next() {
		e, err := scanExpose(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, e)
	}
	return list, rows.Err()
}

// DeleteExpose removes the expose served on the head's port.
func (s *Store) DeleteExpose(ctx context.Context, port int) error {
	err := expectOne(s.db.ExecContext(ctx, `DELETE FROM exposes WHERE port = ?`, port))
	if errors.Is(err, ErrNotFound) {
		return ErrExposeNotFound
	}
	return err
}

func scanExpose(row interface{ Scan(...any) error }) (Expose, error) {
	var e Expose
	var created string
	err := row.Scan(&e.Port, &e.Sandbox, &e.SandboxPort, &e.Public, &e.TokenID, &e.URL, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Expose{}, ErrExposeNotFound
	}
	if err != nil {
		return Expose{}, err
	}
	e.CreatedAt, err = time.Parse(timeFormat, created)
	return e, err
}
