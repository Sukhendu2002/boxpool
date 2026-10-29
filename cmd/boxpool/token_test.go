//go:build linux || darwin

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"boxpool.run/boxpool/internal/auth"
	"boxpool.run/boxpool/internal/daemon"
	"boxpool.run/boxpool/internal/transport"
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
	ca, _, err := auth.LoadOrCreateCA(dir)
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

func TestTokenCreateTailscale(t *testing.T) {
	dir := testDaemon(t)
	bin := t.TempDir()
	fake := filepath.Join(bin, "tailscale")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, tc := range []struct {
		script string
		code   int
		addr   string
		err    string
	}{
		{"echo 100.64.0.7", 0, "100.64.0.7:7443", ""},
		{"echo fd7a:115c:a1e0::7", 0, "[fd7a:115c:a1e0::7]:7443", ""},
		{"echo 'Tailscale is stopped.' >&2; exit 1", 1, "", "run tailscale up"},
		{"echo stopped", 1, "", "not an address"},
	} {
		if err := os.WriteFile(fake, []byte("#!/bin/sh\n[ \"$*\" = 'ip -1' ] || exit 2\n"+tc.script+"\n"), 0755); err != nil {
			t.Fatal(err)
		}
		code, out, errOut := cli(t, dir, "", "token", "create", "--join", "--transport", "tailscale")
		if code != tc.code || !strings.Contains(errOut, tc.err) {
			t.Errorf("%s: token create = %d %q %q, want %d and %q", tc.script, code, out, errOut, tc.code, tc.err)
			continue
		}
		if tc.addr == "" {
			continue
		}
		if tok, err := auth.ParseJoinToken(strings.TrimSpace(out)); err != nil || tok.Addr != tc.addr {
			t.Errorf("%s: token %q: %+v, %v, want address %s", tc.script, out, tok, err, tc.addr)
		}
	}
	if code, _, errOut := cli(t, dir, "", "token", "create", "--join", "--transport", "wireguard"); code != 2 || !strings.Contains(errOut, "unknown --transport") {
		t.Errorf("token create --transport wireguard = %d %q", code, errOut)
	}
	// Explicit addresses and invalid arguments must not invoke discovery.
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 99\n"), 0755); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := cli(t, dir, "", "token", "create", "--join", "--transport", "tailscale", "--addr", "head.example:7443")
	tok, err := auth.ParseJoinToken(strings.TrimSpace(out))
	if code != 0 || err != nil || tok.Addr != "head.example:7443" {
		t.Errorf("explicit address override = %d %q %q, %v", code, out, errOut, err)
	}
	if code, _, errOut := cli(t, dir, "", "token", "create", "--join", "--transport", "tailscale", "--ttl", "0s"); code != 2 || !strings.Contains(errOut, "--ttl must be positive") {
		t.Errorf("invalid ttl before discovery = %d %q", code, errOut)
	}
}

func TestTailnetAddrTimeout(t *testing.T) {
	bin := t.TempDir()
	// exec lets CommandContext kill the sleeping process itself.
	script := "#!/bin/sh\ntouch \"$0.started\"\nexec sleep 30\n"
	fake := filepath.Join(bin, "tailscale")
	if err := os.WriteFile(fake, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	start := time.Now()
	addr, err := (transport.Tailscale{}).Address(context.Background(), auth.JoinPort)
	if addr != "" || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("tailnetAddr = %q, %v; want deadline exceeded", addr, err)
	}
	if elapsed := time.Since(start); elapsed > 8*time.Second {
		t.Fatalf("discovery took %v after its deadline", elapsed)
	}
	if _, err := os.Stat(fake + ".started"); err != nil {
		t.Fatalf("fake tailscale did not start: %v", err)
	}
}
