package client

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"boxpool.run/boxpool/internal/agent"
	"boxpool.run/boxpool/internal/daemon"
)

func TestSocketError(t *testing.T) {
	sock := "/var/lib/boxpool/boxpool.sock"
	for _, tt := range []struct {
		name string
		err  error
		hint string
	}{
		{"missing", syscall.ENOENT, "start it with sudo boxpool up"},
		{"refused", syscall.ECONNREFUSED, "start it with sudo boxpool up"},
		{"permission", syscall.EACCES, "use sudo boxpool <command>"},
		{"operation not permitted", syscall.EPERM, "use sudo boxpool <command>"},
		{"timeout", context.DeadlineExceeded, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dialErr := &net.OpError{Op: "dial", Net: "unix", Err: &os.SyscallError{Syscall: "connect", Err: tt.err}}
			err := socketError(sock, dialErr)
			if !errors.Is(err, tt.err) || !strings.Contains(err.Error(), sock) || !strings.Contains(err.Error(), tt.hint) {
				t.Fatalf("socketError = %v, want socket, cause and hint %q", err, tt.hint)
			}
			if tt.hint == "" || errors.Is(tt.err, os.ErrPermission) {
				if strings.Contains(err.Error(), "start it") || strings.Contains(err.Error(), "boxpool daemon") {
					t.Errorf("unrelated start hint: %v", err)
				}
			}
			if errors.Is(tt.err, os.ErrPermission) {
				for _, want := range []string{"permission denied", "daemon status is unknown", "parent directory"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error = %v, want %q", err, want)
					}
				}
			}
		})
	}
}

func TestMissingSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "bp-client") // Keep within macOS's Unix socket path limit.
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	c := New(dir)
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"health": func() error { return c.Health(ctx) },
		"info":   func() error { _, err := c.Info(ctx); return err },
		"exec": func() error {
			_, err := c.Exec(ctx, "box", agent.ExecRequest{}, io.Discard, io.Discard)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := call()
			if !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), filepath.Join(dir, daemon.SocketName)) || !strings.Contains(err.Error(), "start it with sudo boxpool up") {
				t.Errorf("error = %v, want missing socket and start hint", err)
			}
		})
	}
}
