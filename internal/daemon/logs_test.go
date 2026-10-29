//go:build linux || darwin

package daemon

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"boxpool.run/boxpool/internal/backend"
)

type consoleDriver struct {
	*fakeDriver
	path string
}

func (d *consoleDriver) Logs(ctx context.Context, id string, follow bool) (io.ReadCloser, error) {
	return backend.OpenConsole(ctx, d.path, follow)
}

func TestLogsAPI(t *testing.T) {
	srv, s, d := poolMachine(t, "linux")
	path := filepath.Join(s.info.StateDir, "console.log")
	os.WriteFile(path, []byte("console\n"), 0600)
	s.d = &consoleDriver{d, path}
	post(t, srv, `{"name":"box"}`)
	log, err := OpenLog(s.info.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	log.Write([]byte("daemon\n"))
	for _, tc := range []struct {
		path, want string
		code       int
	}{
		{"/v1/logs", "daemon\n", 200},
		{"/v1/sandboxes/box/logs", "console\n", 200},
		{"/v1/sandboxes/nope/logs", "not found", 404},
		{"/v1/sandboxes/box/logs?follow=oops", "invalid syntax", 400},
	} {
		code, body := call(t, srv, "GET", tc.path)
		if code != tc.code || !strings.Contains(body, tc.want) {
			t.Errorf("GET %s = %d %q", tc.path, code, body)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/v1/logs?follow=true", nil)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b := make([]byte, 7)
	if _, err := io.ReadFull(resp.Body, b); err != nil || string(b) != "daemon\n" {
		t.Fatalf("follow initial: %q, %v", b, err)
	}
	log.Write([]byte("follow\n"))
	if _, err := io.ReadFull(resp.Body, b); err != nil || string(b) != "follow\n" {
		t.Fatalf("follow appended: %q, %v", b, err)
	}
	cancel()
	if _, err := resp.Body.Read(b); err == nil {
		t.Fatal("follow did not end on cancellation")
	}
}

func TestDaemonLogRefusesWritableState(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "untouched")
	if err := os.WriteFile(target, []byte("kept"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, LogName)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0777); err != nil {
		t.Fatal(err)
	}
	if w, err := OpenLog(dir); err == nil {
		w.Close()
		t.Fatal("opened a log in a writable state directory")
	}
	b, err := os.ReadFile(target)
	if err != nil || string(b) != "kept" {
		t.Fatalf("log target changed: %q, %v", b, err)
	}
}

func TestDaemonLogBound(t *testing.T) {
	dir := t.TempDir()
	w, err := OpenLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.Write([]byte(strings.Repeat("x", backend.ConsoleMax+1))); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("latest\n")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, LogName))
	if err != nil || string(b) != "latest\n" {
		t.Fatalf("rotated log = %q, %v", b, err)
	}
	fi, err := os.Stat(filepath.Join(dir, LogName+".1"))
	if err != nil || fi.Size() != backend.ConsoleMax+1 {
		t.Fatalf("previous log = %v, %v", fi, err)
	}
}
