//go:build linux || darwin

package daemon_test

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"boxpool.run/boxpool/internal/client"
	"boxpool.run/boxpool/internal/daemon"
	"boxpool.run/boxpool/internal/preflight"
)

// stateDir returns a state directory that does not exist yet. It avoids
// t.TempDir because macOS limits unix socket paths to about 104 bytes.
func stateDir(t *testing.T) string {
	t.Helper()
	tmp, err := os.MkdirTemp("", "bp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmp) })
	return filepath.Join(tmp, "state")
}

// start runs a daemon on dir and waits until it answers.
func start(t *testing.T, dir string, info daemon.Info) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- daemon.Run(ctx, dir, info) }()
	t.Cleanup(cancel)
	c := client.New(dir)
	for deadline := time.Now().Add(5 * time.Second); c.Health(context.Background()) != nil; {
		select {
		case err := <-done:
			t.Fatalf("daemon exited: %v", err)
		case <-time.After(10 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon did not answer within 5s")
		}
	}
	return func() error {
		cancel()
		return <-done
	}
}

func TestRun(t *testing.T) {
	dir := stateDir(t)
	sock := filepath.Join(dir, daemon.SocketName)
	if err := os.MkdirAll(dir, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sock, nil, 0600); err != nil { // left by a crashed daemon
		t.Fatal(err)
	}
	want := daemon.Info{Version: "test", OS: "linux", Arch: "amd64", StateDir: dir, Backend: "firecracker"}
	stop := start(t, dir, want)

	got, err := client.New(dir).Info(context.Background())
	if err != nil || got != want {
		t.Errorf("Info() = %+v, %v; want %+v", got, err, want)
	}
	fi, err := os.Stat(sock)
	if err != nil || fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0660 {
		t.Errorf("socket = %v, %v; want a socket with mode 0660", fi.Mode(), err)
	}

	if err := stop(); err != nil {
		t.Errorf("Run() = %v after cancel", err)
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Errorf("socket still present after shutdown: %v", err)
	}
}

func TestSecondDaemonRefused(t *testing.T) {
	t.Setenv("BOXPOOL_HOME", stateDir(t))
	dir, err := preflight.StateDir()
	if err != nil {
		t.Fatal(err)
	}
	stop := start(t, dir, daemon.Info{})

	err = daemon.Run(context.Background(), dir, daemon.Info{})
	if err == nil || !strings.Contains(err.Error(), "already running") || !strings.Contains(err.Error(), dir) {
		t.Errorf("second Run() = %v, want an error naming %s", err, dir)
	}
	if err := client.New(dir).Health(context.Background()); err != nil {
		t.Errorf("first daemon broken by the second: %v", err)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	// The lock is released on shutdown, so a new daemon can start.
	start(t, dir, daemon.Info{})
}

func TestRunCancelledBeforeServe(t *testing.T) {
	dir := stateDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := daemon.Run(ctx, dir, daemon.Info{}); err != nil {
		t.Fatal(err)
	}
	// Run has released the lock, so the socket must already be gone: a late
	// close would remove the socket of the next daemon.
	if _, err := os.Stat(filepath.Join(dir, daemon.SocketName)); !os.IsNotExist(err) {
		t.Errorf("socket still present after Run returned: %v", err)
	}
}

func TestShutdownDropsSlowClient(t *testing.T) {
	defer func(d time.Duration) { *daemon.ShutdownTimeout = d }(*daemon.ShutdownTimeout)
	*daemon.ShutdownTimeout = 50 * time.Millisecond
	dir := stateDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- daemon.Run(ctx, dir, daemon.Info{}) }()
	var conn net.Conn
	for deadline := time.Now().Add(5 * time.Second); conn == nil; time.Sleep(10 * time.Millisecond) {
		c, err := net.Dial("unix", filepath.Join(dir, daemon.SocketName))
		if err == nil {
			conn = c
		} else if time.Now().After(deadline) {
			t.Fatal(err)
		}
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("GET /v1/health HTTP/1.1\r\n")); err != nil { // headers never end
		t.Fatal(err)
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Run() = %v, want a shutdown timeout", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("connection still open after Run returned: %v", err)
	}
}

func TestRunRefusesWritableStateDir(t *testing.T) {
	dir := stateDir(t)
	if err := os.MkdirAll(dir, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0770); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // without the check, Run would start and return nil at once
	err := daemon.Run(ctx, dir, daemon.Info{})
	if err == nil || !strings.Contains(err.Error(), "writable by group or others") {
		t.Errorf("Run() = %v, want a refusal", err)
	}
}
