package agent

import (
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// net.FileListener rejects AF_VSOCK sockets, so the listener polls the
// socket through an *os.File, which Go's poller supports for any
// non-blocking descriptor.
type vsockListener struct {
	f    *os.File
	addr vsockAddr
}

type vsockAddr struct{ cid, port uint32 }

func (a vsockAddr) Network() string { return "vsock" }
func (a vsockAddr) String() string  { return fmt.Sprintf("%d:%d", a.cid, a.port) }

// vsockConn is an accepted connection; *os.File provides the I/O and
// deadline methods of net.Conn.
type vsockConn struct {
	*os.File
	local, remote vsockAddr
}

// Read reports errors as *net.OpError, like a socket from package net.
// net/http aborts a read with a deadline when a handler hijacks the
// connection, and cancels the request unless the error is a net.Error.
func (c *vsockConn) Read(b []byte) (int, error) {
	n, err := c.File.Read(b)
	if pe, ok := err.(*os.PathError); ok {
		err = &net.OpError{Op: "read", Net: "vsock", Source: c.local, Addr: c.remote, Err: pe.Err}
	}
	return n, err
}

func (c *vsockConn) LocalAddr() net.Addr  { return c.local }
func (c *vsockConn) RemoteAddr() net.Addr { return c.remote }

func listenVsock(port uint32) (net.Listener, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("vsock socket: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("vsock bind port %d: %w", port, err)
	}
	if err := unix.Listen(fd, unix.SOMAXCONN); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("vsock listen: %w", err)
	}
	return &vsockListener{f: os.NewFile(uintptr(fd), "vsock"), addr: vsockAddr{unix.VMADDR_CID_ANY, port}}, nil
}

func (l *vsockListener) Accept() (net.Conn, error) {
	rc, err := l.f.SyscallConn()
	if err != nil {
		return nil, err
	}
	var (
		nfd  int
		sa   unix.Sockaddr
		aerr error
	)
	if err := rc.Read(func(fd uintptr) bool {
		nfd, sa, aerr = unix.Accept4(int(fd), unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC)
		return aerr != unix.EAGAIN
	}); err != nil {
		return nil, err
	}
	if aerr != nil {
		return nil, aerr
	}
	c := &vsockConn{File: os.NewFile(uintptr(nfd), "vsock"), local: l.addr}
	if vm, ok := sa.(*unix.SockaddrVM); ok {
		c.remote = vsockAddr{vm.CID, vm.Port}
	}
	return c, nil
}

func (l *vsockListener) Close() error   { return l.f.Close() }
func (l *vsockListener) Addr() net.Addr { return l.addr }
