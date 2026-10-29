// Package transport connects pool machines. It carries byte streams;
// join pinning and mutual TLS are layered above it.
package transport

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/netip"
	"os"
)

// Transport provides the head's listener, a node's outbound connections,
// and the local address advertised in join tokens. Closing a listener or
// connection releases it; cancelling ctx interrupts Listen and Dial setup.
type Transport interface {
	Listen(ctx context.Context, addr string) (net.Listener, error)
	Dial(ctx context.Context, addr string) (net.Conn, error)
	Address(ctx context.Context, port string) (string, error)
}

// Direct uses the operating system's TCP sockets on the LAN.
type Direct struct{}

func (Direct) Listen(ctx context.Context, addr string) (net.Listener, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
}

func (Direct) Dial(ctx context.Context, addr string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
}

func (Direct) Address(_ context.Context, port string) (string, error) {
	host, err := os.Hostname()
	if err != nil {
		return "", err
	}
	return net.JoinHostPort(host, port), nil
}

// New selects the transport named by token create's --transport flag.
func New(name string) (Transport, error) {
	switch name {
	case "direct":
		return Direct{}, nil
	case "tailscale":
		return Tailscale{}, nil
	default:
		return nil, fmt.Errorf("unknown transport %q: use direct or tailscale", name)
	}
}

var (
	tailnet4 = netip.MustParsePrefix("100.64.0.0/10")
	tailnet6 = netip.MustParsePrefix("fd7a:115c:a1e0::/48")
)

// ForAddress selects the socket transport for an endpoint. System
// tailscaled handles routing, including DNS aliases and custom Headscale
// prefixes that look like ordinary TCP addresses. Wildcard listeners serve
// both LAN and tailnet interfaces without changing the token wire format.
func ForAddress(addr string) Transport {
	host, _, _ := net.SplitHostPort(addr)
	if ip, err := netip.ParseAddr(host); err == nil && (tailnet4.Contains(ip.Unmap()) || tailnet6.Contains(ip)) {
		return Tailscale{}
	}
	return Direct{}
}

// DialContext adapts pool transports to net/http without moving TLS below
// the transport. Pool endpoints always use TCP.
func DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if network != "tcp" {
		return nil, fmt.Errorf("unsupported pool network %q", network)
	}
	return ForAddress(addr).Dial(ctx, addr)
}

// DialTLS wraps a transport connection with the pool's existing TLS
// configuration. The caller's deadline covers dialing and the handshake.
func DialTLS(ctx context.Context, addr string, cfg *tls.Config) (*tls.Conn, error) {
	c, err := ForAddress(addr).Dial(ctx, addr)
	if err != nil {
		return nil, err
	}
	conn := tls.Client(c, cfg)
	if err := conn.HandshakeContext(ctx); err != nil {
		c.Close()
		return nil, err
	}
	return conn, nil
}
