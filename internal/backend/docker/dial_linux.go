package docker

import (
	"context"
	"fmt"
	"io/fs"
	"net"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

// dialSocket connects to the unix socket at path, never through a symlink
// there. The guest owns the directory the agent socket is in and can put
// anything at its name, say a symlink to another sandbox's socket, which
// the daemon could reach and the guest could not. So path is opened without
// following a symlink, the descriptor is checked to be a socket, and the
// connection goes through /proc/self/fd, which leads to the inode the
// descriptor holds, whatever is at path by then.
func dialSocket(ctx context.Context, path string) (net.Conn, error) {
	fd, err := openSocket(path)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	return dialFD(ctx, fd, path)
}

// dialFD connects to the socket fd, from openSocket, holds; path is for
// errors.
func dialFD(ctx context.Context, fd int, path string) (net.Conn, error) {
	c, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", "/proc/self/fd/"+strconv.Itoa(fd))
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", path, err)
	}
	return c, nil
}

// openSocket returns an O_PATH descriptor of the socket at path, or an
// error if path is a symlink or anything else that is not a socket.
func openSocket(path string) (int, error) {
	fd, err := unix.Open(path, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return -1, &fs.PathError{Op: "stat", Path: path, Err: err}
	}
	if st.Mode&unix.S_IFMT != unix.S_IFSOCK {
		unix.Close(fd)
		return -1, fmt.Errorf("%s is not a unix socket (mode %#o): refusing to connect", path, st.Mode&unix.S_IFMT)
	}
	return fd, nil
}
