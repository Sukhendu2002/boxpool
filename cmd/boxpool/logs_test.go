//go:build linux || darwin

package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"boxpool.run/boxpool/internal/client"
	"boxpool.run/boxpool/internal/daemon"
)

func (d *fakeDriver) Logs(ctx context.Context, id string, follow bool) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("sandbox console\n")), nil
}

func TestLogsCLI(t *testing.T) {
	dir := testDaemon(t)
	if err := os.WriteFile(filepath.Join(dir, daemon.LogName), []byte("daemon output\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := client.New(dir).CreateSandbox(context.Background(), daemon.CreateRequest{Name: "box"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"logs", "--daemon"}, 0, "daemon output\n"},
		{[]string{"logs", "box"}, 0, "sandbox console\n"},
		{[]string{"logs", "missing"}, 1, ""},
		{[]string{"logs"}, 2, ""},
		{[]string{"logs", "--daemon", "box"}, 2, ""},
		{[]string{"logs", "a", "b"}, 2, ""},
		{[]string{"logs", "--bogus"}, 2, ""},
	} {
		code, out, stderr := cli(t, dir, "", tc.args...)
		if code != tc.code || out != tc.want {
			t.Errorf("%v = %d %q %q", tc.args, code, out, stderr)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r, err := client.New(dir).Logs(ctx, "", true)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	b := make([]byte, len("daemon output\n"))
	if _, err := io.ReadFull(r, b); err != nil || string(b) != "daemon output\n" {
		t.Fatalf("client follow = %q, %v", b, err)
	}
	cancel()
	if _, err := r.Read(b); err == nil {
		t.Fatal("client follow ignored cancellation")
	}
}
