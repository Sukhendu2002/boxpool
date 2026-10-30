package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"
)

// FeatureTCP in Version.Features means the agent serves GET /v1/tcp.
const FeatureTCP = "tcp"

// tcpProtocol is the Upgrade token of GET /v1/tcp.
const tcpProtocol = "boxpool-tcp"

// connectTCP connects to a port on the guest's loopback and, once it is
// connected, switches the request's connection to carry its bytes.
func connectTCP(w http.ResponseWriter, r *http.Request) {
	port, err := strconv.Atoi(r.URL.Query().Get("port"))
	if err != nil || port < 1 || port > 65535 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("bad port %q", r.URL.Query().Get("port")))
		return
	}
	var up net.Conn
	// Dev servers asked for localhost may listen on either address.
	for _, host := range []string{"127.0.0.1", "::1"} {
		if up, err = net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 5*time.Second); err == nil {
			break
		}
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Errorf("nothing accepts connections on port %d: %w", port, err))
		return
	}
	defer up.Close()
	c, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	defer c.Close()
	// Hijacked connections outlive Serve unless its context closes them.
	stop := context.AfterFunc(r.Context(), func() { c.Close(); up.Close() })
	defer stop()
	brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + tcpProtocol + "\r\n\r\n")
	if brw.Flush() != nil {
		return
	}
	go func() {
		io.Copy(up, brw.Reader)
		if tc, ok := up.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}()
	io.Copy(c, up)
}

// DialTCP asks the agent at the other end of c, a new connection to it, to
// connect to port on the guest's loopback, and returns c carrying that
// connection. It closes c if the agent refuses.
func DialTCP(ctx context.Context, c net.Conn, port int) (net.Conn, error) {
	stop := context.AfterFunc(ctx, func() { c.SetDeadline(time.Now()) })
	defer stop()
	req, _ := http.NewRequest(http.MethodGet, "http://agent/v1/tcp?port="+strconv.Itoa(port), nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", tcpProtocol)
	br := bufio.NewReader(c)
	err := req.Write(c)
	var resp *http.Response
	if err == nil {
		resp, err = http.ReadResponse(br, req)
	}
	if err == nil && resp.StatusCode != http.StatusSwitchingProtocols {
		var e errorResponse
		json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&e)
		err = fmt.Errorf("GET /v1/tcp: %s: %s", resp.Status, e.Message)
		if resp.StatusCode == http.StatusNotFound {
			err = fmt.Errorf("the sandbox's agent cannot connect to ports: update its image")
		}
	}
	if err == nil && !stop() {
		err = ctx.Err()
	}
	if err != nil {
		c.Close()
		return nil, err
	}
	return &bufConn{Conn: c, r: br}, nil
}

// bufConn is a net.Conn whose reads start with what r already read from it.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufConn) Read(b []byte) (int, error) { return c.r.Read(b) }
