//go:build !linux

package agent

import (
	"errors"
	"net"
)

func listenVsock(port uint32) (net.Listener, error) {
	return nil, errors.New("vsock is only supported on Linux")
}
