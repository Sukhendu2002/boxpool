//go:build linux || darwin

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"boxpool.run/boxpool/internal/auth"
	"boxpool.run/boxpool/internal/store"
)

func TestAuditCmd(t *testing.T) {
	dir := testDaemon(t)
	if code, out, errOut := cli(t, dir, "", "audit", "--json"); code != 0 || strings.TrimSpace(out) != "[]" || errOut != "" {
		t.Fatalf("empty audit = %d %q %q", code, out, errOut)
	}
	code, out, errOut := cli(t, dir, "", "token", "create", "--scope", "cli")
	tok, err := auth.ParseAccessToken(strings.TrimSpace(out))
	if code != 0 || err != nil {
		t.Fatalf("create token = %d %q %q: %v", code, out, errOut, err)
	}
	id := tok.Hash()[:store.AccessIDLen]
	if code, _, errOut := cli(t, dir, "", "token", "revoke", id); code != 0 {
		t.Fatalf("revoke token = %d %q", code, errOut)
	}
	// Even a saved, unreachable remote login must leave audit on the head.
	if err := os.WriteFile(filepath.Join(dir, "login.json"), []byte(`{"head":"127.0.0.1:1","token":"`+tok.String()+`"}`), 0600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut = cli(t, dir, "", "audit")
	if code != 0 || errOut != "" || !strings.Contains(out, "TIME") || !strings.Contains(out, "local") || !strings.Contains(out, "token.create") || !strings.Contains(out, "token.revoke") || !strings.Contains(out, id) || strings.Contains(out, tok.String()) {
		t.Fatalf("audit table = %d %q %q", code, out, errOut)
	}
	code, out, errOut = cli(t, dir, "", "audit", "--json", "--limit", "1")
	var list []store.AuditEvent
	if json.Unmarshal([]byte(out), &list) != nil || code != 0 || len(list) != 1 || list[0].Action != "token.revoke" || list[0].Target != id {
		t.Fatalf("latest audit = %d %q %q", code, out, errOut)
	}
	code, out, errOut = cli(t, dir, "", "audit", "--json", "--before", fmt.Sprint(list[0].ID))
	if json.Unmarshal([]byte(out), &list) != nil || code != 0 || len(list) != 1 || list[0].Action != "token.create" || list[0].Target != id {
		t.Fatalf("older audit = %d %q %q", code, out, errOut)
	}
	for _, args := range [][]string{{"audit", "extra"}, {"audit", "--limit", "0"}, {"audit", "--limit", "1001"}, {"audit", "--before", "-1"}, {"audit", "--unknown"}} {
		if code, _, _ := cli(t, dir, "", args...); code != 2 {
			t.Errorf("%v = %d, want 2", args, code)
		}
	}
	if code, _, errOut := cli(t, t.TempDir(), "", "audit"); code != 1 || !strings.Contains(errOut, "cannot reach the daemon") {
		t.Errorf("no daemon = %d %q", code, errOut)
	}
}
