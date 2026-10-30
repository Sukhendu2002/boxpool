//go:build linux || darwin

package main

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"boxpool.run/boxpool/internal/agent"
)

func TestTCPImageProbe(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("image TCP probe needs Node.js, which the guest image includes")
	}
	dir, err := os.MkdirTemp("", "bptcp")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "agent.sock")
	l, err := agent.Listen("unix:" + sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	done := make(chan error, 1)
	go func() { done <- agent.Serve(ctx, l, "test", nil) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	dial := func(ctx context.Context) (net.Conn, error) { return (&net.Dialer{}).DialContext(ctx, "unix", sock) }
	c := agent.NewClient(dial)
	defer c.CloseIdleConnections()
	if err := checkTCP(ctx, c, dial); err != nil {
		t.Fatal(err)
	}
}
