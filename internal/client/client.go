// Package client talks to the local Boxpool daemon over its unix socket,
// or to a head over the network with an access token.
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
	"boxpool.run/boxpool/internal/auth"
	"boxpool.run/boxpool/internal/daemon"
	"boxpool.run/boxpool/internal/transport"
)

// Client is a client for the daemon serving one state directory, or for
// a remote head.
type Client struct {
	hc    *http.Client
	base  string // URL the API's paths are relative to
	token string // access token for a remote head
}

// New returns a client for the daemon whose state directory is stateDir.
func New(stateDir string) *Client {
	sock := filepath.Join(stateDir, daemon.SocketName)
	// The host is ignored: every request goes to the socket.
	return &Client{base: "http://boxpool", hc: &http.Client{Transport: &http.Transport{
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

// Remote returns a client for the head at addr, host:port, with an access
// token, which also pins the head's CA.
func Remote(addr, token string) (*Client, error) {
	tok, err := auth.ParseAccessToken(token)
	if err != nil {
		return nil, err
	}
	tr := &http.Transport{TLSClientConfig: tok.TLS(), DialContext: transport.DialContext}
	return &Client{base: "https://" + addr, token: token, hc: &http.Client{Transport: tr}}, nil
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

// CreateJoinToken returns a new single-use join token for this node's pool.
func (c *Client) CreateJoinToken(ctx context.Context, req daemon.TokenRequest) (daemon.Token, error) {
	var tok daemon.Token
	body, _ := json.Marshal(req)
	err := c.do(ctx, http.MethodPost, "/v1/tokens", bytes.NewReader(body), &tok)
	return tok, err
}

// RevokeJoinToken makes an unused join token invalid. Only its hash is
// sent to the daemon.
func (c *Client) RevokeJoinToken(ctx context.Context, token string) error {
	t, err := auth.ParseJoinToken(token)
	if err != nil {
		return err
	}
	resp, err := c.send(ctx, http.MethodDelete, "/v1/tokens/"+t.Hash(), "", nil)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// CreateAccessToken returns a new access token for this head's API.
func (c *Client) CreateAccessToken(ctx context.Context, req daemon.AccessTokenRequest) (daemon.AccessToken, error) {
	var tok daemon.AccessToken
	body, _ := json.Marshal(req)
	err := c.do(ctx, http.MethodPost, "/v1/access-tokens", bytes.NewReader(body), &tok)
	return tok, err
}

// ListAccessTokens returns this head's access tokens, oldest first.
func (c *Client) ListAccessTokens(ctx context.Context) ([]daemon.AccessToken, error) {
	var list []daemon.AccessToken
	err := c.get(ctx, "/v1/access-tokens", &list)
	return list, err
}

// RevokeAccessToken makes the access token with the given ID invalid.
func (c *Client) RevokeAccessToken(ctx context.Context, id string) error {
	resp, err := c.send(ctx, http.MethodDelete, "/v1/access-tokens/"+url.PathEscape(id), "", nil)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// JoinPool joins this node to the pool named in token.
func (c *Client) JoinPool(ctx context.Context, token string) (daemon.JoinPoolResponse, error) {
	var out daemon.JoinPoolResponse
	body, _ := json.Marshal(daemon.JoinPoolRequest{Token: token})
	err := c.do(ctx, http.MethodPost, "/v1/pool/join", bytes.NewReader(body), &out)
	return out, err
}

// Promote makes this machine its pool's head, from its replica of the
// pool's state, once the old head is gone.
func (c *Client) Promote(ctx context.Context) (daemon.PromoteResponse, error) {
	var out daemon.PromoteResponse
	err := c.do(ctx, http.MethodPost, "/v1/pool/promote", nil, &out)
	return out, err
}

// ListNodes returns the head of this machine's pool and its nodes.
func (c *Client) ListNodes(ctx context.Context) ([]daemon.Node, error) {
	var list []daemon.Node
	err := c.get(ctx, "/v1/nodes", &list)
	return list, err
}

// SetLabels replaces this machine's labels and returns them as stored.
func (c *Client) SetLabels(ctx context.Context, labels []string) ([]string, error) {
	var out daemon.Labels
	body, _ := json.Marshal(daemon.Labels{Labels: labels})
	err := c.do(ctx, http.MethodPut, "/v1/labels", bytes.NewReader(body), &out)
	return out.Labels, err
}

// Logs reads a sandbox's console, or this daemon's log when name is empty.
// Close the returned stream to stop following it.
func (c *Client) Logs(ctx context.Context, name string, follow bool) (io.ReadCloser, error) {
	path := "/v1/logs"
	if name != "" {
		path = "/v1/sandboxes/" + url.PathEscape(name) + "/logs"
	}
	if follow {
		path += "?follow=true"
	}
	resp, err := c.send(ctx, http.MethodGet, path, "", nil)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// Exec starts a command in the named sandbox through the daemon, which
// relays the agent's exec protocol unchanged.
func (c *Client) Exec(ctx context.Context, name string, req agent.ExecRequest, stdout, stderr io.Writer) (*agent.Session, error) {
	path := "/v1/sandboxes/" + url.PathEscape(name) + "/exec"
	conn, resp, err := websocket.Dial(ctx, c.base+path, &websocket.DialOptions{HTTPClient: c.hc, HTTPHeader: c.header()})
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
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header = c.header()
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

func (c *Client) header() http.Header {
	h := http.Header{}
	if c.token != "" {
		h.Set("Authorization", "Bearer "+c.token)
	}
	return h
}

// apiError returns the daemon's error message, or the status without one.
func apiError(method, path string, resp *http.Response) error {
	var e daemon.Error
	if json.NewDecoder(io.LimitReader(resp.Body, 1024)).Decode(&e) == nil && e.Message != "" {
		return errors.New(e.Message)
	}
	return fmt.Errorf("%s %s: %s", method, path, resp.Status)
}
