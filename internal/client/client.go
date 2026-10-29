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
	"os"
	"path/filepath"
	"syscall"

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
			conn, err := d.DialContext(ctx, "unix", sock)
			if err != nil {
				return nil, socketError(sock, err)
			}
			return conn, nil
		},
	}}}
}

// socketError adds actionable context without losing the underlying dial error.
func socketError(sock string, err error) error {
	switch {
	case errors.Is(err, os.ErrPermission):
		return fmt.Errorf("permission denied accessing daemon socket %s (daemon status is unknown: the socket or a parent directory is inaccessible; use sudo boxpool <command> for a root daemon, or run as the daemon's user): %w", sock, err)
	case errors.Is(err, os.ErrNotExist), errors.Is(err, syscall.ECONNREFUSED):
		return fmt.Errorf("cannot reach the daemon at %s (start it with sudo boxpool up on Linux with systemd, or boxpool daemon in the foreground): %w", sock, err)
	default:
		return fmt.Errorf("cannot connect to daemon socket %s: %w", sock, err)
	}
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

// StartSandbox boots the named sandbox from its kept disk.
func (c *Client) StartSandbox(ctx context.Context, name string) (daemon.Sandbox, error) {
	var sb daemon.Sandbox
	err := c.do(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(name)+"/start", nil, &sb)
	return sb, err
}

// StopSandbox stops the named sandbox. The result's Warning is set if it
// did not shut down cleanly.
func (c *Client) StopSandbox(ctx context.Context, name string) (daemon.Sandbox, error) {
	var sb daemon.Sandbox
	err := c.do(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(name)+"/stop", nil, &sb)
	return sb, err
}

// PauseSandbox pauses the named sandbox. The result's Warning is set if
// the backend stopped it instead, losing its memory.
func (c *Client) PauseSandbox(ctx context.Context, name string) (daemon.Sandbox, error) {
	var sb daemon.Sandbox
	err := c.do(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(name)+"/pause", nil, &sb)
	return sb, err
}

// ResumeSandbox resumes the named paused sandbox.
func (c *Client) ResumeSandbox(ctx context.Context, name string) (daemon.Sandbox, error) {
	var sb daemon.Sandbox
	err := c.do(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(name)+"/resume", nil, &sb)
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

// SnapshotSandbox copies the named sandbox's disk to a new snapshot called
// snapshot. The daemon stops a running sandbox for the copy and starts it
// again. The result's Warning is set if it did not stop cleanly.
func (c *Client) SnapshotSandbox(ctx context.Context, name, snapshot string) (daemon.Snapshot, error) {
	var snap daemon.Snapshot
	body, _ := json.Marshal(daemon.SnapshotRequest{Name: snapshot})
	err := c.do(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(name)+"/snapshot", bytes.NewReader(body), &snap)
	return snap, err
}

// ListSnapshots returns every snapshot on this node, oldest first.
func (c *Client) ListSnapshots(ctx context.Context) ([]daemon.Snapshot, error) {
	var list []daemon.Snapshot
	err := c.get(ctx, "/v1/snapshots", &list)
	return list, err
}

// RemoveSnapshot deletes the named snapshot. Sandboxes started from it are
// not affected.
func (c *Client) RemoveSnapshot(ctx context.Context, name string) (daemon.Snapshot, error) {
	var snap daemon.Snapshot
	err := c.do(ctx, http.MethodDelete, "/v1/snapshots/"+url.PathEscape(name), nil, &snap)
	return snap, err
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
	resp, err := c.send(ctx, method, path, "application/json", body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(v)
}

// Upload extracts a tar stream into the existing directory dest in the
// named sandbox.
func (c *Client) Upload(ctx context.Context, name, dest string, tar io.Reader) error {
	resp, err := c.send(ctx, http.MethodPut, filesPath(name, dest), "application/x-tar", tar)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// Download returns src in the named sandbox as a tar rooted at its base
// name. The tar comes from the guest, so it is untrusted. The caller must
// read to EOF to detect interrupted transfers, then close it.
func (c *Client) Download(ctx context.Context, name, src string) (io.ReadCloser, error) {
	resp, err := c.send(ctx, http.MethodGet, filesPath(name, src), "", nil)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func filesPath(name, path string) string {
	return "/v1/sandboxes/" + url.PathEscape(name) + "/files?path=" + url.QueryEscape(path)
}

// send sends a request and returns the response if it succeeded, or the
// daemon's error.
func (c *Client) send(ctx context.Context, method, path, contentType string, body io.Reader) (*http.Response, error) {
	// The host is ignored: every request goes to the socket.
	req, err := http.NewRequestWithContext(ctx, method, "http://boxpool"+path, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		return nil, apiError(method, path, resp)
	}
	return resp, nil
}

// apiError returns the daemon's error message, or the status without one.
func apiError(method, path string, resp *http.Response) error {
	var e daemon.Error
	if json.NewDecoder(io.LimitReader(resp.Body, 1024)).Decode(&e) == nil && e.Message != "" {
		return errors.New(e.Message)
	}
	return fmt.Errorf("%s %s: %s", method, path, resp.Status)
}
