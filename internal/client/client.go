// Package client talks to the local Boxpool daemon over its unix socket.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"

	"github.com/coder/websocket"

	"boxpool.run/boxpool/internal/agent"
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

// CreateSandbox returns the sandbox named req.Name, started, creating it if
// there is none.
func (c *Client) CreateSandbox(ctx context.Context, req daemon.CreateRequest) (daemon.Sandbox, error) {
	var sb daemon.Sandbox
	body, _ := json.Marshal(req)
	err := c.do(ctx, http.MethodPost, "/v1/sandboxes", bytes.NewReader(body), &sb)
	return sb, err
}

// GetSandbox returns the sandbox with the given name.
func (c *Client) GetSandbox(ctx context.Context, name string) (daemon.Sandbox, error) {
	var sb daemon.Sandbox
	err := c.get(ctx, "/v1/sandboxes/"+url.PathEscape(name), &sb)
	return sb, err
}

// ListSandboxes returns every sandbox on this node, oldest first.
func (c *Client) ListSandboxes(ctx context.Context) ([]daemon.Sandbox, error) {
	var list []daemon.Sandbox
	err := c.get(ctx, "/v1/sandboxes", &list)
	return list, err
}

// StopSandbox stops the named sandbox. The result's Warning is set if it
// did not shut down cleanly.
func (c *Client) StopSandbox(ctx context.Context, name string) (daemon.Sandbox, error) {
	var sb daemon.Sandbox
	err := c.do(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(name)+"/stop", nil, &sb)
	return sb, err
}

// RemoveSandbox deletes the named sandbox and frees its resources. Without
// force the daemon refuses a running sandbox; with it, it stops it first.
func (c *Client) RemoveSandbox(ctx context.Context, name string, force bool) (daemon.Sandbox, error) {
	var sb daemon.Sandbox
	path := "/v1/sandboxes/" + url.PathEscape(name)
	if force {
		path += "?force=1"
	}
	err := c.do(ctx, http.MethodDelete, path, nil, &sb)
	return sb, err
}

// Exec starts a command in the named sandbox through the daemon, which
// relays the agent's exec protocol unchanged.
func (c *Client) Exec(ctx context.Context, name string, req agent.ExecRequest, stdout, stderr io.Writer) (*agent.Session, error) {
	path := "/v1/sandboxes/" + url.PathEscape(name) + "/exec"
	conn, resp, err := websocket.Dial(ctx, "ws://boxpool"+path, &websocket.DialOptions{HTTPClient: c.hc})
	if err != nil {
		if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
			return nil, apiError(http.MethodGet, path, resp)
		}
		return nil, err
	}
	return agent.Start(ctx, conn, req, stdout, stderr)
}

func (c *Client) get(ctx context.Context, path string, v any) error {
	return c.do(ctx, http.MethodGet, path, nil, v)
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, v any) error {
	// The host is ignored: every request goes to the socket.
	req, err := http.NewRequestWithContext(ctx, method, "http://boxpool"+path, body)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return apiError(method, path, resp)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// apiError returns the daemon's error message, or the status without one.
func apiError(method, path string, resp *http.Response) error {
	var e daemon.Error
	if json.NewDecoder(io.LimitReader(resp.Body, 1024)).Decode(&e) == nil && e.Message != "" {
		return errors.New(e.Message)
	}
	return fmt.Errorf("%s %s: %s", method, path, resp.Status)
}
