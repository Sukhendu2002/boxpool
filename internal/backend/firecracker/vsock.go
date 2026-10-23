package firecracker

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

// dialAgent connects to the guest agent through the host end of a
// Firecracker vsock device.
func dialAgent(ctx context.Context, socket string) (net.Conn, error) {
	c, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, err
	}
	// Firecracker's protocol is CONNECT <guest port>, answered by OK <port>.
	// Keep the reader: it may have buffered data after the answer.
	c.SetDeadline(time.Now().Add(time.Second))
	if _, err = fmt.Fprintf(c, "CONNECT %d\n", agentPort); err != nil {
		c.Close()
		return nil, err
	}
	r := bufio.NewReader(c)
	line, err := r.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "OK ") {
		c.Close()
		return nil, fmt.Errorf("vsock: CONNECT %d answered %q: %v", agentPort, line, err)
	}
	c.SetDeadline(time.Time{})
	return &bufferedConn{Conn: c, r: r}, nil
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }
