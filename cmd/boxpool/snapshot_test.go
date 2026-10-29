//go:build linux || darwin

package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"boxpool.run/boxpool/internal/client"
	"boxpool.run/boxpool/internal/daemon"
)

func TestSnapshotCmd(t *testing.T) {
	dir := testDaemon(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := client.New(dir).CreateSandbox(ctx, daemon.CreateRequest{Name: "box"}); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := cli(t, dir, "", "snapshot", "box", "--name", "withjq"); code != 0 || errOut != "" {
		t.Errorf("snapshot = %d %q", code, errOut)
	}
	if code, _, errOut := cli(t, dir, "", "snapshot", "--name", "withjq", "box"); code != 1 || !strings.Contains(errOut, `snapshot name "withjq" already exists`) {
		t.Errorf("second snapshot = %d %q", code, errOut)
	}
	code, out, _ := cli(t, dir, "", "snapshot", "ls")
	if code != 0 || !strings.HasPrefix(out, "NAME     SANDBOX   BACKEND   DISK   AGE\nwithjq   box       fake      10g    ") {
		t.Errorf("snapshot ls = %d %q", code, out)
	}
	code, out, _ = cli(t, dir, "", "snapshot", "ls", "--json")
	var list []daemon.Snapshot
	if err := json.Unmarshal([]byte(out), &list); code != 0 || err != nil || len(list) != 1 || list[0].Name != "withjq" {
		t.Errorf("snapshot ls --json = %d %s", code, out)
	}
	if code, _, errOut := cli(t, dir, "", "snapshot", "rm", "withjq"); code != 0 || errOut != "" {
		t.Errorf("snapshot rm = %d %q", code, errOut)
	}
	if code, _, errOut := cli(t, dir, "", "snapshot", "rm", "withjq"); code != 1 || !strings.Contains(errOut, `snapshot "withjq" not found`) {
		t.Errorf("second snapshot rm = %d %q", code, errOut)
	}
	for _, args := range [][]string{{"snapshot"}, {"snapshot", "box"}, {"snapshot", "--name", "x"}, {"snapshot", "box", "--name", "x", "y"}, {"snapshot", "rm"}, {"snapshot", "ls", "x"}} {
		if code, _, _ := cli(t, dir, "", args...); code != 2 {
			t.Errorf("%v = %d, want 2", args, code)
		}
	}
}
