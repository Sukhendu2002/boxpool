package agent

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
	"time"

	"github.com/coder/websocket"
)

// Client talks to one agent.
type Client struct {
	hc *http.Client
}

// NewClient returns a client that reaches the agent through dial, for
// example over a unix socket or a vsock connection.
func NewClient(dial func(ctx context.Context) (net.Conn, error)) *Client {
	return &Client{hc: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) },
	}}}
}

// CloseIdleConnections closes connections kept open for reuse, which
// otherwise stay open to the agent for as long as the process runs.
func (c *Client) CloseIdleConnections() { c.hc.CloseIdleConnections() }

// Version returns the agent's protocol and build version.
func (c *Client) Version(ctx context.Context) (Version, error) {
	var v Version
	// The host is ignored: every request goes through dial.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://agent/v1/version", nil)
	if err != nil {
		return v, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return v, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return v, fmt.Errorf("GET /v1/version: %s", resp.Status)
	}
	return v, json.NewDecoder(resp.Body).Decode(&v)
}

// Shutdown asks the agent to stop every guest process, giving them grace
// after SIGTERM before SIGKILL, remount / read-only and reboot the guest.
// It returns once / is read-only; the reboot follows, and can cut the reply
// short.
func (c *Client) Shutdown(ctx context.Context, grace time.Duration) error {
	b, _ := json.Marshal(ShutdownRequest{GraceMS: grace.Milliseconds()})
	resp, err := c.request(ctx, http.MethodPost, "/v1/shutdown", bytes.NewReader(b))
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// Session is a running command. Cancelling the context passed to Exec ends
// the session, and the agent kills the command.
type Session struct {
	ctx  context.Context
	conn *websocket.Conn
	done chan struct{}
	code int
	err  error
}

// Exec starts a command and copies its output to stdout and stderr until
// it exits. With req.TTY all output goes to stdout.
func (c *Client) Exec(ctx context.Context, req ExecRequest, stdout, stderr io.Writer) (*Session, error) {
	conn, _, err := websocket.Dial(ctx, "ws://agent/v1/exec", &websocket.DialOptions{HTTPClient: c.hc})
	if err != nil {
		return nil, err
	}
	return Start(ctx, conn, req, stdout, stderr)
}

// Start runs an exec session on conn, an open exec WebSocket to an agent or
// to something that relays one, such as the daemon. The session owns conn.
func Start(ctx context.Context, conn *websocket.Conn, req ExecRequest, stdout, stderr io.Writer) (*Session, error) {
	b, err := json.Marshal(req)
	if err == nil {
		err = conn.Write(ctx, websocket.MessageText, b)
	}
	if err != nil {
		conn.CloseNow()
		return nil, err
	}
	s := &Session{ctx: ctx, conn: conn, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		s.code, s.err = s.receive(stdout, stderr)
		if s.err != nil {
			conn.CloseNow()
		} else {
			conn.Close(websocket.StatusNormalClosure, "")
		}
	}()
	return s, nil
}

func (s *Session) receive(stdout, stderr io.Writer) (int, error) {
	for {
		typ, b, err := s.conn.Read(s.ctx)
		if err != nil {
			return 0, fmt.Errorf("agent connection lost before the command exited: %w", err)
		}
		if typ == websocket.MessageBinary {
			var w io.Writer
			switch {
			case len(b) == 0:
			case b[0] == Stdout:
				w = stdout
			case b[0] == Stderr:
				w = stderr
			}
			if w != nil {
				if _, err := w.Write(b[1:]); err != nil {
					return 0, err
				}
			}
			continue
		}
		var m message
		if err := json.Unmarshal(b, &m); err != nil {
			return 0, fmt.Errorf("bad message from agent: %w", err)
		}
		switch m.Type {
		case "exit":
			if m.Code == nil {
				return 0, errors.New("agent sent exit without a code")
			}
			return *m.Code, nil
		case "error":
			return 0, fmt.Errorf("agent: %s", m.Message)
		}
	}
}

// Write sends p to the command's stdin.
func (s *Session) Write(p []byte) (int, error) {
	for n := 0; n < len(p); {
		chunk := p[n:min(len(p), n+maxMessage-1)]
		if err := s.conn.Write(s.ctx, websocket.MessageBinary, append([]byte{Stdin}, chunk...)); err != nil {
			return n, err
		}
		n += len(chunk)
	}
	return len(p), nil
}

// CloseStdin closes the command's stdin. It does nothing in TTY mode, where
// the client sends Ctrl-D instead.
func (s *Session) CloseStdin() error { return s.send(message{Type: "stdin_eof"}) }

// Resize sets the PTY's window size. It does nothing outside TTY mode.
func (s *Session) Resize(rows, cols uint16) error {
	return s.send(message{Type: "resize", Rows: rows, Cols: cols})
}

// Wait waits for the command to exit and returns its exit code.
func (s *Session) Wait() (int, error) {
	<-s.done
	return s.code, s.err
}

func (s *Session) send(m message) error {
	b, _ := json.Marshal(m)
	return s.conn.Write(s.ctx, websocket.MessageText, b)
}

// Upload extracts a tar stream into the existing directory dest.
func (c *Client) Upload(ctx context.Context, dest string, tar io.Reader) error {
	resp, err := c.request(ctx, http.MethodPut, "/v1/files?path="+url.QueryEscape(dest), tar)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// Download returns a tar rooted at the base name of src. The caller must
// read to EOF to detect interrupted transfers, then close the body.
func (c *Client) Download(ctx context.Context, src string) (io.ReadCloser, error) {
	resp, err := c.request(ctx, http.MethodGet, "/v1/files?path="+url.QueryEscape(src), nil)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// Ports returns a snapshot of the guest's listening TCP sockets.
func (c *Client) Ports(ctx context.Context) ([]Port, error) {
	resp, err := c.request(ctx, http.MethodGet, "/v1/ports", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var ports []Port
	err = json.NewDecoder(resp.Body).Decode(&ports)
	return ports, err
}

func (c *Client) request(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://agent"+path, body)
	if err != nil {
		return nil, err
	}
	if method == http.MethodPut {
		req.Header.Set("Content-Type", "application/x-tar")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	want := http.StatusOK
	if method == http.MethodPut {
		want = http.StatusNoContent
	}
	if resp.StatusCode != want {
		defer resp.Body.Close()
		var e errorResponse
		json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&e)
		return nil, fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, e.Message)
	}
	return resp, nil
}
