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
	"boxpool.run/boxpool/internal/store"
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
		{[]string{"token", "revoke", "bpjoin1_nonsense"}, 1, "not a boxpool join token"},
		{[]string{"token", "revoke", "nonsense"}, 1, "no access token with that ID"},
		{[]string{"token", "create", "--join", "--scope", "cli"}, 2, "Usage:"},
		{[]string{"token", "create", "--scope", "cli", "--addr", "head.lan:7443"}, 2, "Usage:"},
		{[]string{"token", "create", "--join", "--sandbox", "web"}, 2, "Usage:"},
		{[]string{"token", "create", "--scope", "root"}, 1, `invalid scope "root"`},
	} {
		if code, out, errOut := cli(t, dir, "", tc.args...); code != tc.code || out != "" || !strings.Contains(errOut, tc.err) {
			t.Errorf("%v = %d %q %q, want %d, no token, and %q", tc.args, code, out, errOut, tc.code, tc.err)
		}
	}
}

func TestAccessTokenCmd(t *testing.T) {
	dir := testDaemon(t)
	create := func(args ...string) (string, string) {
		t.Helper()
		code, out, errOut := cli(t, dir, "", append([]string{"token", "create"}, args...)...)
		if code != 0 {
			t.Fatalf("token create %v = %d %q", args, code, errOut)
		}
		tok, err := auth.ParseAccessToken(strings.TrimSpace(out))
		if err != nil {
			t.Fatalf("token %q: %v", out, err)
		}
		return tok.String(), errOut
	}
	cliTok, msg := create("--scope", "cli")
	if !strings.Contains(msg, "never expires") || !strings.Contains(msg, "boxpool login") {
		t.Errorf("token create --scope cli said %q", msg)
	}
	webTok, msg := create("--scope", "expose", "--sandbox", "web", "--ttl", "1h")
	if !strings.Contains(msg, "; expires ") {
		t.Errorf("token create --ttl 1h said %q", msg)
	}
	web, _ := auth.ParseAccessToken(webTok)
	webID := web.Hash()[:store.AccessIDLen]
	code, out, errOut := cli(t, dir, "", "token", "ls")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != 0 || len(lines) != 3 || !strings.HasPrefix(lines[0], "ID") || !strings.Contains(lines[1], "cli") || !strings.Contains(lines[2], webID) || !strings.Contains(lines[2], "web") || strings.Contains(out, cliTok) {
		t.Errorf("token ls = %d %q %q", code, out, errOut)
	}
	if code, _, errOut := cli(t, dir, "", "token", "revoke", webID); code != 0 {
		t.Errorf("token revoke <id> = %d %q", code, errOut)
	}
	if code, _, errOut := cli(t, dir, "", "token", "revoke", cliTok); code != 0 {
		t.Errorf("token revoke <token> = %d %q", code, errOut)
	}
	if code, out, _ := cli(t, dir, "", "token", "ls"); code != 0 || strings.Count(out, "\n") != 1 {
		t.Errorf("token ls after revoking both = %d %q", code, out)
	}
}

func TestLogin(t *testing.T) {
	dir := testDaemon(t)
	tok := auth.NewAccessToken([32]byte{}).String()
	for _, tc := range []struct {
		args []string
		code int
		err  string
	}{
		{[]string{"login", "head"}, 2, "Usage:"},
		{[]string{"login", "head", "bpjoin1_x"}, 2, "not a boxpool access token"},
		{[]string{"login", "head:0", tok}, 2, "invalid head address"},
		{[]string{"login", "127.0.0.1:1", tok}, 1, "127.0.0.1:1"},
	} {
		if code, _, errOut := cli(t, dir, "", tc.args...); code != tc.code || !strings.Contains(errOut, tc.err) {
			t.Errorf("%v = %d %q, want %d and %q", tc.args, code, errOut, tc.code, tc.err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "login.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a failed login saved itself: %v", err)
	}
	// Once logged in, sandbox commands go to the head; machine commands do not.
	if err := os.WriteFile(filepath.Join(dir, "login.json"), []byte(`{"head":"127.0.0.1:1","token":"`+tok+`"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := cli(t, dir, "", "ls"); code != 1 || !strings.Contains(errOut, "127.0.0.1:1") {
		t.Errorf("ls after login = %d %q, want it to reach the head", code, errOut)
	}
	if code, _, errOut := cli(t, dir, "", "token", "ls"); code != 0 {
		t.Errorf("token ls after login = %d %q, want the local daemon", code, errOut)
	}
	if code, _, errOut := cli(t, dir, "", "logout"); code != 0 {
		t.Fatalf("logout = %d %q", code, errOut)
	}
	if code, _, errOut := cli(t, dir, "", "ls"); code != 0 {
		t.Errorf("ls after logout = %d %q", code, errOut)
	}
	if code, _, errOut := cli(t, dir, "", "logout"); code != 0 {
		t.Errorf("second logout = %d %q", code, errOut)
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
