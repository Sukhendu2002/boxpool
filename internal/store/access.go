package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// AccessIDLen is the length of an access token's ID: the start of its
// hash, which token ls shows and token revoke takes.
const AccessIDLen = 12

// AccessToken is an access token the head issued. A zero ExpiresAt or
// LastUsed means never.
//
// shortcut: access tokens live only on the head that issued them and are
// not replicated with raft, so a promoted head has none; add them to
// PoolState when users run pools of two or more.
type AccessToken struct {
	Hash      string
	Scope     string
	Sandbox   string
	CreatedAt time.Time
	ExpiresAt time.Time
	LastUsed  time.Time
}

// ID is the start of the token's hash.
func (t AccessToken) ID() string { return t.Hash[:AccessIDLen] }

// CreateAccessToken stores t.
func (s *Store) CreateAccessToken(ctx context.Context, t AccessToken) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO access_tokens (hash, scope, sandbox, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
		t.Hash, t.Scope, t.Sandbox, t.CreatedAt.UTC().Format(timeFormat), formatOptional(t.ExpiresAt))
	return err
}

const selectAccessToken = `hash, scope, sandbox, created_at, expires_at, last_used`

// UseAccessToken returns the access token with the given hash and records
// now as its last use. It returns ErrTokenInvalid for an unknown or
// revoked token, and ErrTokenExpired after it expires.
func (s *Store) UseAccessToken(ctx context.Context, hash string, now time.Time) (AccessToken, error) {
	t, err := scanAccessToken(s.db.QueryRowContext(ctx, `UPDATE access_tokens SET last_used = ? WHERE hash = ? RETURNING `+selectAccessToken,
		now.UTC().Format(timeFormat), hash))
	if err == nil && !t.ExpiresAt.IsZero() && !now.Before(t.ExpiresAt) {
		return AccessToken{}, ErrTokenExpired
	}
	return t, err
}

// ListAccessTokens returns every access token, oldest first.
func (s *Store) ListAccessTokens(ctx context.Context) ([]AccessToken, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+selectAccessToken+` FROM access_tokens ORDER BY created_at, hash`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []AccessToken
	for rows.Next() {
		t, err := scanAccessToken(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, t)
	}
	return list, rows.Err()
}

// RevokeAccessToken deletes the access token with the given ID.
func (s *Store) RevokeAccessToken(ctx context.Context, id string) error {
	if len(id) != AccessIDLen {
		return ErrTokenInvalid
	}
	err := expectOne(s.db.ExecContext(ctx, `DELETE FROM access_tokens WHERE substr(hash, 1, ?) = ?`, AccessIDLen, id))
	if errors.Is(err, ErrNotFound) {
		return ErrTokenInvalid
	}
	return err
}

func scanAccessToken(row interface{ Scan(...any) error }) (AccessToken, error) {
	var t AccessToken
	var created, expires, used string
	err := row.Scan(&t.Hash, &t.Scope, &t.Sandbox, &created, &expires, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return AccessToken{}, ErrTokenInvalid
	}
	if err != nil {
		return AccessToken{}, err
	}
	if t.CreatedAt, err = time.Parse(timeFormat, created); err != nil {
		return AccessToken{}, err
	}
	if t.ExpiresAt, err = parseOptional(expires); err != nil {
		return AccessToken{}, err
	}
	t.LastUsed, err = parseOptional(used)
	return t, err
}

func formatOptional(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(timeFormat)
}

func parseOptional(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(timeFormat, s)
}
