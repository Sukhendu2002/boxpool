package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAccessTokens(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	ctx := context.Background()
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	forever := AccessToken{Hash: strings.Repeat("a", 64), Scope: "cli", CreatedAt: t0}
	brief := AccessToken{Hash: strings.Repeat("b", 64), Scope: "expose", Sandbox: "web", CreatedAt: t0.Add(time.Second), ExpiresAt: t0.Add(time.Hour)}
	for _, tok := range []AccessToken{forever, brief} {
		if err := s.CreateAccessToken(ctx, tok); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.UseAccessToken(ctx, brief.Hash, t0.Add(time.Minute))
	if brief.LastUsed = t0.Add(time.Minute); err != nil || got != brief {
		t.Errorf("UseAccessToken = %+v, %v; want %+v", got, err, brief)
	}
	if _, err := s.UseAccessToken(ctx, brief.Hash, t0.Add(time.Hour)); !errors.Is(err, ErrTokenExpired) {
		t.Errorf("UseAccessToken at expiry = %v, want ErrTokenExpired", err)
	}
	if _, err := s.UseAccessToken(ctx, forever.Hash, t0.AddDate(10, 0, 0)); err != nil {
		t.Errorf("a token without expiry = %v", err)
	}
	list, err := s.ListAccessTokens(ctx)
	if err != nil || len(list) != 2 || list[0].Hash != forever.Hash || list[1].LastUsed.IsZero() {
		t.Errorf("ListAccessTokens = %+v, %v", list, err)
	}

	for _, id := range []string{"", "aaaa", forever.Hash, strings.Repeat("c", AccessIDLen)} {
		if err := s.RevokeAccessToken(ctx, id); !errors.Is(err, ErrTokenInvalid) {
			t.Errorf("RevokeAccessToken(%q) = %v, want ErrTokenInvalid", id, err)
		}
	}
	if err := s.RevokeAccessToken(ctx, forever.ID()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UseAccessToken(ctx, forever.Hash, t0); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("a revoked token = %v, want ErrTokenInvalid", err)
	}
	if list, _ := s.ListAccessTokens(ctx); len(list) != 1 || list[0].Hash != brief.Hash {
		t.Errorf("ListAccessTokens after revoke = %+v", list)
	}
}
