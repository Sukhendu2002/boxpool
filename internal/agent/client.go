package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"

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
