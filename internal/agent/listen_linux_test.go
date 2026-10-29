package agent

import (
	"context"
	"net"
	"os"
	"strconv"
	"testing"

	"golang.org/x/sys/unix"
)

// TestVsock needs the vsock_loopback transport, which lets a host talk to
// itself on CID 1. Without it, vsock is only testable inside a VM.
func TestVsock(t *testing.T) {
	port := uint32(20000 + os.Getpid()%10000)
	l, err := Listen("vsock:" + strconv.Itoa(int(port)))
	if err != nil {
		t.Skipf("no vsock here: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, l, "test", nil) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve() = %v", err)
		}
	}()
	c := NewClient(func(context.Context) (net.Conn, error) {
		fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			return nil, err
		}
		if err := unix.Connect(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_LOCAL, Port: port}); err != nil {
			unix.Close(fd)
			return nil, err
		}
		unix.SetNonblock(fd, true)
		return &vsockConn{File: os.NewFile(uintptr(fd), "vsock")}, nil
	})
	if _, err := c.Version(ctx); err != nil {
		t.Skipf("no vsock loopback here: %v", err)
	}
	var out output
	s, err := c.Exec(ctx, ExecRequest{Argv: []string{"echo", "over vsock"}}, &out, &out)
	if err != nil {
		t.Fatal(err)
	}
	if code, err := s.Wait(); err != nil || code != 0 || out.String() != "over vsock\n" {
		t.Errorf("exec over vsock = %q, %d, %v", out.String(), code, err)
	}
}
