package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAudit(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	s := open(t, path)
	now := time.Now().UTC().Truncate(time.Second)
	tok := AccessToken{Hash: strings.Repeat("a", 64), Scope: "cli", CreatedAt: now}
	if err := s.CreateAccessToken(ctx, tok); err != nil {
		t.Fatal(err)
	}
	events := []AuditEvent{
		{Time: now.Add(-AuditRetention - time.Second), TokenID: "local", Action: "token.create", Target: "expired"},
		{Time: now.Add(-AuditRetention + time.Hour), TokenID: tok.ID(), Action: "sandbox.create", Target: "web"},
		{Time: now, TokenID: tok.ID(), Action: "sandbox.rm", Target: "web"},
		{Time: now, TokenID: "local", Action: "token.revoke", Target: tok.ID()},
	}
	if err := s.AppendAudit(ctx, events...); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeAccessToken(ctx, tok.ID()); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListAudit(ctx, 0, 2)
	if err != nil || len(list) != 2 || list[0].Action != "token.revoke" || list[1].Action != "sandbox.rm" || !list[1].Time.Equal(now) {
		t.Fatalf("latest = %+v, %v", list, err)
	}
	before := list[1].ID
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = open(t, path)
	list, err = s.ListAudit(ctx, before, 100)
	if err != nil || len(list) != 1 || list[0].TokenID != tok.ID() || list[0].Target != "web" {
		t.Fatalf("older after reopen and revoke = %+v, %v", list, err)
	}
	// Simulate time passing on an otherwise idle head. A read prunes too.
	if _, err := s.db.Exec(`UPDATE audit_events SET time = ?`, now.Add(-AuditRetention-time.Second).Format(timeFormat)); err != nil {
		t.Fatal(err)
	}
	list, err = s.ListAudit(ctx, 0, 100)
	if err != nil || list == nil || len(list) != 0 {
		t.Fatalf("retained = %+v, %v; want an empty array", list, err)
	}
	for _, tc := range []struct {
		before int64
		limit  int
	}{{-1, 100}, {0, 0}, {0, 1001}} {
		if _, err := s.ListAudit(ctx, tc.before, tc.limit); err == nil {
			t.Errorf("ListAudit(%d, %d) accepted invalid bounds", tc.before, tc.limit)
		}
	}
}

func TestAuditBatchIsAtomic(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	if _, err := s.db.Exec(`CREATE TRIGGER reject_audit BEFORE INSERT ON audit_events WHEN NEW.action = 'fail' BEGIN SELECT RAISE(ABORT, 'test failure'); END`); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.AppendAudit(ctx, AuditEvent{Time: time.Now(), Action: "first"}, AuditEvent{Time: time.Now(), Action: "fail"}); err == nil {
		t.Fatal("batch should fail")
	}
	list, err := s.ListAudit(ctx, 0, 100)
	if err != nil || len(list) != 0 {
		t.Fatalf("partial batch = %+v, %v", list, err)
	}
}
