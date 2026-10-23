//go:build !linux

package docker

import (
	"context"
	"net"
	"time"
)

// The fake-engine tests talk to their fake agents through a plain dial,
// which follows symlinks; dial_linux_test.go tests the real one on Linux.
func init() {
	dialSocket = func(ctx context.Context, path string) (net.Conn, error) {
		return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", path)
	}
}
