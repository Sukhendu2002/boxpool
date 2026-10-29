//go:build linux || darwin

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"boxpool.run/boxpool/internal/auth"
	"boxpool.run/boxpool/internal/daemon"
)

func TestTokenCmd(t *testing.T) {
	dir := testDaemon(t)
	code, out, errOut := cli(t, dir, "", "token", "create", "--join", "--ttl", "10m", "--addr", "head.lan:7443")
	if code != 0 || !strings.HasPrefix(errOut, "Single use; expires ") {
		t.Fatalf("token create = %d %q %q", code, out, errOut)
	}
	tok, err := auth.ParseJoinToken(strings.TrimSpace(out))
	if err != nil || tok.Addr != "head.lan:7443" {
		t.Fatalf("token %q: %+v, %v", out, tok, err)
	}
	ca, err := auth.LoadOrCreateCA(dir)
	if err != nil || tok.CA != sha256.Sum256(ca.Raw) {
		t.Errorf("token does not pin the head CA: %v", err)
	}
	var db []byte
	for _, f := range []string{daemon.StoreName, daemon.StoreName + "-wal"} {
		b, _ := os.ReadFile(filepath.Join(dir, f))
		db = append(db, b...)
	}
	if !bytes.Contains(db, []byte(tok.Hash())) || bytes.Contains(db, tok.Secret[:]) || bytes.Contains(db, []byte(hex.EncodeToString(tok.Secret[:]))) {
		t.Error("state.db should hold the token's hash and not its secret")
	}

	if code, _, errOut := cli(t, dir, "", "token", "revoke", tok.String()); code != 0 || errOut != "" {
		t.Errorf("token revoke = %d %q", code, errOut)
	}
	if code, _, errOut := cli(t, dir, "", "token", "revoke", tok.String()); code != 1 || !strings.Contains(errOut, "token is invalid or already used") {
		t.Errorf("second token revoke = %d %q", code, errOut)
	}
	for _, tc := range []struct {
		args []string
		code int
		err  string
	}{
		{[]string{"token", "create"}, 2, "Usage:"},
		{[]string{"token", "create", "--join", "--ttl", "0s"}, 2, "--ttl must be positive"},
		{[]string{"token", "create", "--join", "--addr", "head.lan"}, 1, `invalid head address "head.lan"`},
		{[]string{"token", "create", "--join", "--addr", "127.0.0.1:65536"}, 1, `invalid head address "127.0.0.1:65536"`},
		{[]string{"token", "create", "--join", "--addr", "127.0.0.1:-1"}, 1, `invalid head address "127.0.0.1:-1"`},
		{[]string{"token", "create", "--join", "--addr", "head.lan:0"}, 1, `invalid head address "head.lan:0"`},
		{[]string{"token", "create", "--join", "--addr", "head.lan:https"}, 1, `invalid head address "head.lan:https"`},
		{[]string{"token", "revoke", "nonsense"}, 1, "not a boxpool join token"},
	} {
		if code, out, errOut := cli(t, dir, "", tc.args...); code != tc.code || out != "" || !strings.Contains(errOut, tc.err) {
			t.Errorf("%v = %d %q %q, want %d, no token, and %q", tc.args, code, out, errOut, tc.code, tc.err)
		}
	}
}
