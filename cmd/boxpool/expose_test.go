//go:build linux || darwin

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"boxpool.run/boxpool/internal/client"
	"boxpool.run/boxpool/internal/daemon"
)

func TestExposeCmd(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\n[ \"$1\" = status ] && echo '{\"Self\":{\"DNSName\":\"head.tail1.ts.net.\"}}'\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "tailscale"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	dir := testDaemon(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := client.New(dir).CreateSandbox(ctx, daemon.CreateRequest{Name: "box"}); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := cli(t, dir, "", "expose", "box", "8000")
	if code != 0 || !strings.HasPrefix(out, "https://head.tail1.ts.net:") || !strings.Contains(out, "/?boxpool_token=bpat1_") || !strings.Contains(errOut, "expose rm box 8000") {
		t.Errorf("expose = %d %q %q", code, out, errOut)
	}
	if code, out, _ := cli(t, dir, "", "expose", "box", "3000", "--public"); code != 0 || strings.Contains(out, "?") {
		t.Errorf("expose --public = %d %q", code, out)
	}
	code, out, _ = cli(t, dir, "", "expose", "ls")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != 0 || len(lines) != 3 || !strings.HasPrefix(lines[1], "box       8000   https://head.tail1.ts.net:") || !strings.Contains(lines[1], "token ") || !strings.Contains(lines[2], "public") {
		t.Errorf("expose ls = %d %q", code, out)
	}
	if code, _, errOut := cli(t, dir, "", "expose", "rm", "box", "8000"); code != 0 || errOut != "" {
		t.Errorf("expose rm = %d %q", code, errOut)
	}
	if code, _, errOut := cli(t, dir, "", "expose", "rm", "box", "8000"); code != 1 || !strings.Contains(errOut, "not exposed") {
		t.Errorf("second expose rm = %d %q", code, errOut)
	}
	for _, args := range [][]string{{"expose"}, {"expose", "box"}, {"expose", "box", "http"}, {"expose", "box", "1", "2"}, {"expose", "rm", "box"}, {"expose", "ls", "x"}} {
		if code, _, _ := cli(t, dir, "", args...); code != 2 {
			t.Errorf("%v = %d, want 2", args, code)
		}
	}
}
