// Package client talks to the local Boxpool daemon over its unix socket.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"

	"boxpool.run/boxpool/internal/daemon"
)

// Client is a client for the daemon serving one state directory.
type Client struct {
	hc *http.Client
}

// New returns a client for the daemon whose state directory is stateDir.
func New(stateDir string) *Client {
	sock := filepath.Join(stateDir, daemon.SocketName)
	return &Client{hc: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}}
}

// Health returns nil when the daemon reports itself healthy.
func (c *Client) Health(ctx context.Context) error {
	var h struct{ OK bool }
	if err := c.get(ctx, "/v1/health", &h); err != nil {
		return err
	}
	if !h.OK {
		return errors.New("daemon is not healthy")
	}
	return nil
}

// Info returns the daemon's description of itself and its node.
func (c *Client) Info(ctx context.Context) (daemon.Info, error) {
	var info daemon.Info
	err := c.get(ctx, "/v1/info", &info)
	return info, err
}

func (c *Client) get(ctx context.Context, path string, v any) error {
	// The host is ignored: every request goes to the socket.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://boxpool"+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}
