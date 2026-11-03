package store

import (
	"context"
	"fmt"
	"time"
)

// AuditRetention is how long this head keeps audit events.
const AuditRetention = 90 * 24 * time.Hour

// AuditEvent records a successful action. TokenID is an access token ID,
// "local" for the local socket, or "public" for an unauthenticated exposed
// URL. Targets contain names and IDs, never token secrets or command text.
// Events live only on the head that recorded them, without raft replication.
type AuditEvent struct {
	ID      int64     `json:"id"`
	Time    time.Time `json:"time"`
	TokenID string    `json:"token_id"`
	Action  string    `json:"action"`
	Target  string    `json:"target"`
}

// AppendAudit appends events and expires old rows in one transaction. There
// is no update or individual-delete API. Retention is the only deletion.
func (s *Store) AppendAudit(ctx context.Context, events ...AuditEvent) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, e := range events {
		if _, err := tx.ExecContext(ctx, `INSERT INTO audit_events (time, token_id, action, target) VALUES (?, ?, ?, ?)`,
			e.Time.UTC().Format(timeFormat), e.TokenID, e.Action, e.Target); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM audit_events WHERE time < ?`, time.Now().UTC().Add(-AuditRetention).Format(timeFormat)); err != nil {
		return err
	}
	return tx.Commit()
}

// ListAudit returns up to limit events, newest first, before the given ID
// (zero means latest). Reads also expire old rows, including on an idle head.
func (s *Store) ListAudit(ctx context.Context, before int64, limit int) ([]AuditEvent, error) {
	if before < 0 || limit < 1 || limit > 1000 {
		return nil, fmt.Errorf("audit: before must be nonnegative and limit must be between 1 and 1000")
	}
	if err := s.AppendAudit(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, time, token_id, action, target FROM audit_events WHERE (? = 0 OR id < ?) ORDER BY id DESC LIMIT ?`, before, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []AuditEvent{}
	for rows.Next() {
		var e AuditEvent
		var ts string
		if err := rows.Scan(&e.ID, &ts, &e.TokenID, &e.Action, &e.Target); err != nil {
			return nil, err
		}
		if e.Time, err = time.Parse(timeFormat, ts); err != nil {
			return nil, err
		}
		list = append(list, e)
	}
	return list, rows.Err()
}
