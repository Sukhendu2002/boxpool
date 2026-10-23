//go:build !linux

package docker

import (
	"context"
	"errors"
	"net"
)

// dialSocket fails: only Linux can connect to the socket a path names
// without following a symlink there (see dial_linux.go), and New refuses
// other hosts anyway. A variable so that the fake-engine tests can run on
// macOS; they do not test that defence.
var dialSocket = func(ctx context.Context, path string) (net.Conn, error) {
	return nil, errors.New("the docker backend needs a Linux host")
}
