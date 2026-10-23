//go:build linux || darwin

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"boxpool.run/boxpool/internal/client"
	"boxpool.run/boxpool/internal/daemon"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// golden compares got with testdata/name, or rewrites it with -update.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs (go test -run %s -update rewrites it):\ngot:\n%s\nwant:\n%s", path, t.Name(), got, want)
	}
}

// cli runs the boxpool command without a terminal, with stdin, against the
// daemon in dir.
func cli(t *testing.T, dir, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), "BOXPOOL_TEST_MAIN=1", "BOXPOOL_HOME="+dir)
	var out, errOut bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader(stdin), &out, &errOut
	err := cmd.Run()
	if _, ok := err.(*exec.ExitError); err != nil && !ok {
		t.Fatal(err)
	}
	return cmd.ProcessState.ExitCode(), out.String(), errOut.String()
}

// testDaemon starts a test daemon in a new short state directory.
func testDaemon(t *testing.T) string {
	t.Helper()
	tmp, err := os.MkdirTemp("", "bp") // short: macOS caps socket paths at ~104 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmp) })
	startDaemon(t, tmp)
	return tmp
}

var lsFixture = func() []daemon.Sandbox {
	created := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	return []daemon.Sandbox{
		{ID: "0a1b2c", Name: "web", Backend: "firecracker", State: "running", CPUs: 2, MemoryMB: 2048, DiskGB: 10, CreatedAt: created},
		{ID: "3d4e5f", Name: "sb-0c9a7e", Backend: "firecracker", State: "stopped", CPUs: 1, MemoryMB: 512, DiskGB: 1, CreatedAt: created.Add(3*24*time.Hour + 5*time.Hour)},
		{ID: "6a7b8c", Name: "old-docker-box", Backend: "docker", State: daemon.StateMissing, CPUs: 4, MemoryMB: 1536, DiskGB: 20, CreatedAt: created.Add(4*24*time.Hour - 90*time.Second)},
	}
}()

func TestLsTable(t *testing.T) {
	var b bytes.Buffer
	writeTable(&b, lsFixture, time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC))
	golden(t, "ls.txt", b.Bytes())
}

func TestLsJSON(t *testing.T) {
	var b bytes.Buffer
	writeJSONList(&b, lsFixture)
	golden(t, "ls.json", b.Bytes())
	b.Reset()
	writeJSONList(&b, nil)
	if b.String() != "[]\n" {
		t.Errorf("no sandboxes = %q, want []", b.String())
	}
}

func TestAge(t *testing.T) {
	for d, want := range map[time.Duration]string{
		-time.Second: "0s", 0: "0s", 59 * time.Second: "59s", time.Minute: "1m", 59 * time.Minute: "59m",
		time.Hour: "1h", 47 * time.Hour: "47h", 48 * time.Hour: "2d", 400 * 24 * time.Hour: "400d",
	} {
		if got := age(d); got != want {
			t.Errorf("age(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestLsStopRm(t *testing.T) {
	dir := testDaemon(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := client.New(dir)
	for _, name := range []string{"a", "b"} {
		if _, err := c.CreateSandbox(ctx, daemon.CreateRequest{Name: name, CPUs: 1, MemoryMB: 512}); err != nil {
			t.Fatal(err)
		}
	}
	code, out, errOut := cli(t, dir, "", "ls")
	if code != 0 || !strings.HasPrefix(out, "NAME   BACKEND   STATE     CPU   MEMORY   AGE\na      fake      running   1     512m     ") || !strings.Contains(out, "\nb      fake      running") {
		t.Errorf("ls = %d %q %q", code, out, errOut)
	}

	if code, _, errOut := cli(t, dir, "", "stop", "a"); code != 0 || errOut != "" {
		t.Errorf("stop a = %d %q", code, errOut)
	}
	if code, _, errOut := cli(t, dir, "", "rm", "b"); code != 1 || !strings.Contains(errOut, `sandbox "b" is running: stop it first`) {
		t.Errorf("rm b = %d %q", code, errOut)
	}
	if code, _, errOut := cli(t, dir, "", "rm", "--force", "b"); code != 0 || errOut != "" {
		t.Errorf("rm --force b = %d %q", code, errOut)
	}
	code, out, _ = cli(t, dir, "", "ls", "--json")
	var list []daemon.Sandbox
	if err := json.Unmarshal([]byte(out), &list); code != 0 || err != nil || len(list) != 1 || list[0].Name != "a" || list[0].State != "stopped" {
		t.Errorf("ls --json = %d %s", code, out)
	}
	if code, _, errOut := cli(t, dir, "", "rm", "nope"); code != 1 || !strings.Contains(errOut, `sandbox "nope" not found`) {
		t.Errorf("rm nope = %d %q", code, errOut)
	}
	for _, args := range [][]string{{"ls", "x"}, {"stop"}, {"rm", "a", "b"}, {"rm", "--bogus", "a"}} {
		if code, _, _ := cli(t, dir, "", args...); code != 2 {
			t.Errorf("%v = %d, want 2", args, code)
		}
	}
}
