//go:build linux || darwin

package agent

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// serveSocket runs an agent on a unix socket and returns its path.
func serveSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "bpa")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "agent.sock")
	l, err := Listen("unix:" + sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, l, "test", nil) }()
	t.Cleanup(func() { cancel(); <-done })
	return sock
}

func TestDialTCP(t *testing.T) {
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	sock := serveSocket(t)
	ctx := testContext(t)
	dial := func(port int) (net.Conn, error) {
		c, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		return DialTCP(ctx, c, port)
	}

	c, err := dial(echo.Addr().(*net.TCPAddr).Port)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "hello" {
		t.Errorf("echo = %q, %v; want hello", buf, err)
	}

	// A port nothing listens on is an error before the switch.
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	port := closed.Addr().(*net.TCPAddr).Port
	closed.Close()
	if _, err := dial(port); err == nil || !strings.Contains(err.Error(), "502") {
		t.Errorf("DialTCP to closed port %d = %v; want a 502", port, err)
	}
	if _, err := dial(0); err == nil || !strings.Contains(err.Error(), "bad port") {
		t.Errorf("DialTCP to port 0 = %v; want bad port", err)
	}
}
