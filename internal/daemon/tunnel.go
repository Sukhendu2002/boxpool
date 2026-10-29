package daemon

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"boxpool.run/boxpool/internal/auth"
	"boxpool.run/boxpool/internal/transport"
)

// A node keeps tunnelsPerNode requests to GET /v1/tunnel waiting on the
// head, so it needs no open port. When the head has a request for the node,
// it answers one with 101 and sends the request over that connection;
// otherwise it answers 204 after tunnelPoll and the node asks again. A
// forwarded request waits up to tunnelWait for a tunnel.
var (
	tunnelPoll = 15 * time.Second
	tunnelWait = 5 * time.Second
)

const tunnelsPerNode = 4

// tunnels pairs the head's forwarded requests with nodes' waiting tunnels.
type tunnels struct {
	mu   sync.Mutex
	want map[string]chan chan net.Conn // by node ID
}

func (t *tunnels) wanted(id string) chan chan net.Conn {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.want == nil {
		t.want = map[string]chan chan net.Conn{}
	}
	ch, ok := t.want[id]
	if !ok {
		ch = make(chan chan net.Conn)
		t.want[id] = ch
	}
	return ch
}

// take returns a connection to node id's API.
func (t *tunnels) take(ctx context.Context, id string) (net.Conn, error) {
	want := make(chan net.Conn, 1)
	wait := time.NewTimer(tunnelWait)
	defer wait.Stop()
	select {
	case t.wanted(id) <- want:
	case <-wait.C:
		return nil, errors.New("it has no connection open to the head")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if c := <-want; c != nil {
		return c, nil
	}
	return nil, errors.New("its tunnel failed")
}

// tunnel is the head's GET /v1/tunnel: it holds a node's request until a
// forwarded request needs the connection, or tunnelPoll passes.
func (s *server) tunnel(w http.ResponseWriter, r *http.Request) {
	poll := time.NewTimer(tunnelPoll)
	defer poll.Stop()
	select {
	case want := <-s.tunnels.wanted(nodeID(r)):
		c, brw, err := http.NewResponseController(w).Hijack()
		if err == nil {
			c.SetWriteDeadline(time.Now().Add(tunnelWait))
			brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + tunnelProtocol + "\r\n\r\n")
			if err = brw.Flush(); err != nil {
				c.Close()
			} else {
				c.SetDeadline(time.Time{})
			}
		}
		if err != nil {
			slog.Warn("tunnel failed", "node", nodeID(r), "err", err)
			c = nil
		}
		want <- c
	case <-poll.C:
		w.WriteHeader(http.StatusNoContent)
	case <-s.poolDone:
		w.WriteHeader(http.StatusNoContent)
	case <-r.Context().Done():
	}
}

const tunnelProtocol = "boxpool-tunnel"

// nodeID is the ID of the node whose verified certificate sent r.
func nodeID(r *http.Request) string {
	return r.TLS.PeerCertificates[0].Subject.CommonName
}

// headListener is a node's listener for the head's requests: each
// connection is a tunnel the head switched to and sends one request on.
type headListener struct {
	ctx   context.Context
	stop  context.CancelFunc
	cfg   *tls.Config
	head  func() string // "" while this machine is the head
	conns chan net.Conn
}

func listenHead(ctx context.Context, cfg *tls.Config, head func() string) *headListener {
	ctx, stop := context.WithCancel(ctx)
	l := &headListener{ctx: ctx, stop: stop, cfg: cfg, head: head, conns: make(chan net.Conn)}
	for range tunnelsPerNode {
		go l.poll()
	}
	return l
}

func (l *headListener) poll() {
	for l.ctx.Err() == nil {
		c, err := l.open()
		if err != nil {
			select { // the heartbeat logs that the head is unreachable
			case <-time.After(2 * time.Second):
			case <-l.ctx.Done():
			}
			continue
		}
		select {
		case l.conns <- c:
		case <-l.ctx.Done():
			c.Close()
		}
	}
}

// open dials the head and asks for a tunnel until the head switches one.
func (l *headListener) open() (net.Conn, error) {
	head := l.head()
	if head == "" {
		return nil, errors.New("this machine is the head")
	}
	ctx, cancel := context.WithTimeout(l.ctx, 10*time.Second)
	c, err := transport.DialTLS(ctx, head, l.cfg)
	cancel()
	if err != nil {
		return nil, err
	}
	unwatch := context.AfterFunc(l.ctx, func() { c.Close() })
	br := bufio.NewReader(c)
	for {
		// A sleeping head or broken connection must not strand all pollers.
		c.SetDeadline(time.Now().Add(tunnelPoll + 10*time.Second))
		req, _ := http.NewRequest(http.MethodGet, "https://"+auth.HeadName+"/v1/tunnel", nil)
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", tunnelProtocol)
		if err = req.Write(c); err != nil {
			break
		}
		var resp *http.Response
		if resp, err = http.ReadResponse(br, req); err != nil {
			break
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusSwitchingProtocols && unwatch() {
			c.SetDeadline(time.Time{})
			return &bufConn{Conn: c, r: br}, nil
		}
		if resp.StatusCode != http.StatusNoContent {
			err = fmt.Errorf("the head answered %s", resp.Status)
			break
		}
	}
	unwatch()
	c.Close()
	return nil, err
}

func (l *headListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.ctx.Done():
		return nil, net.ErrClosed
	}
}

func (l *headListener) Close() error   { l.stop(); return nil }
func (l *headListener) Addr() net.Addr { return headAddr(l.head()) }

type headAddr string

func (a headAddr) Network() string { return "tcp" }
func (a headAddr) String() string  { return string(a) }

// bufConn is a net.Conn whose reads start with what r already read from it.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufConn) Read(b []byte) (int, error) { return c.r.Read(b) }
