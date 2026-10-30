// Command smoke checks the built guest over Firecracker's host-side vsock UDS.
// It is deliberately image-specific, not a backend driver or test framework.
package main

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"boxpool.run/boxpool/internal/agent"
)

func main() {
	socket := flag.String("socket", "", "Firecracker vsock UDS path")
	version := flag.String("version", "", "expected image and agent version")
	flag.Parse()
	if *socket == "" || *version == "" {
		fmt.Fprintln(os.Stderr, "-socket and -version are required")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := check(ctx, *socket, *version); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func dial(ctx context.Context, socket string) (net.Conn, error) {
	c, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, err
	}
	// Firecracker's UDS protocol is CONNECT <guest port> followed by OK <port>.
	// Keep the reader: it may have buffered data after the acknowledgement.
	c.SetDeadline(time.Now().Add(time.Second))
	if _, err = fmt.Fprint(c, "CONNECT 1024\n"); err != nil {
		c.Close()
		return nil, err
	}
	r := bufio.NewReader(c)
	line, err := r.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "OK ") {
		c.Close()
		return nil, fmt.Errorf("vsock acknowledgement %q: %v", line, err)
	}
	c.SetDeadline(time.Time{})
	return &bufferedConn{Conn: c, reader: r}, nil
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

func check(ctx context.Context, socket, version string) error {
	c := agent.NewClient(func(ctx context.Context) (net.Conn, error) { return dial(ctx, socket) })
	var v agent.Version
	var err error
	for {
		v, err = c.Version(ctx)
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("agent did not become ready: %w (last error: %v)", ctx.Err(), err)
		case <-time.After(100 * time.Millisecond):
		}
	}
	if v.Protocol != agent.ProtocolVersion || v.Agent != version {
		return fmt.Errorf("unexpected version: %+v, want protocol %d agent %s", v, agent.ProtocolVersion, version)
	}
	if !slices.Contains(v.Features, agent.FeatureTCP) {
		return fmt.Errorf("image agent lacks tcp feature: %+v", v)
	}
	fmt.Printf("GET /v1/version: protocol=%d agent=%s\n", v.Protocol, v.Agent)
	var out bytes.Buffer
	s, err := c.Exec(ctx, agent.ExecRequest{Argv: []string{"sh", "-ec", `
test -t 0
test -r /proc/net/tcp
test -r /proc/net/tcp6
test "$(nproc)" -eq 1
memory=$(free -m)
printf '%s\n' "$memory"
printf '%s\n' "$memory" | awk '/^Mem:/ {found=1; if ($2 < 409 || $2 > 512) exit 1} END {if (!found) exit 1}'
dev_mounts=0
while read -r _ target fstype options _; do
    test "$target" = /dev || continue
    test "$fstype" = devtmpfs
    case ,$options, in *,nosuid,*) ;; *) exit 1 ;; esac
    dev_mounts=$((dev_mounts + 1))
done < /proc/mounts
test "$dev_mounts" = 1
test -c /dev/null
test -c /dev/ptmx
test -d /dev/pts
test "$(printf stdin-ok | cat /dev/stdin)" = stdin-ok
test "$(printf fd-ok | cat /dev/fd/0)" = fd-ok
sh -ec 'printf stdout-ok > /dev/stdout; printf stderr-ok > /dev/stderr' > /tmp/image-smoke.stdout 2> /tmp/image-smoke.stderr
test "$(cat /tmp/image-smoke.stdout)" = stdout-ok
test "$(cat /tmp/image-smoke.stderr)" = stderr-ok
rm /tmp/image-smoke.stdout /tmp/image-smoke.stderr
test "$(cat /proc/1/comm)" = tini
test "$(stat -c %a /tmp)" = 1777
test "$(stat -c %a /var/tmp)" = 1777
test "$(stat -c %u /root)" = 0
test "$(readlink /bin)" = usr/bin
test "$(cat /etc/boxpool-image)" = "$EXPECTED_VERSION"
claude --version
codex --version
gemini --version
touch /tmp/image-smoke /var/tmp/image-smoke /run/boxpool/image-smoke
rm /tmp/image-smoke /var/tmp/image-smoke /run/boxpool/image-smoke
sh -c 'sleep 0.2 </dev/null >/dev/null 2>&1 & echo $! > /tmp/image-orphan.pid'
sleep 1
test ! -e "/proc/$(cat /tmp/image-orphan.pid)"
rm /tmp/image-orphan.pid
echo image-smoke-ok
`}, Env: []string{"EXPECTED_VERSION=" + version, "TERM=xterm"}, TTY: true}, &out, &out)
	if err != nil {
		return err
	}
	code, err := s.Wait()
	if err != nil || code != 0 || !strings.Contains(out.String(), "image-smoke-ok") {
		return fmt.Errorf("guest exec/PTY/init check: exit=%d err=%v output=%q", code, err, out.String())
	}
	if _, err := c.Ports(ctx); err != nil {
		return fmt.Errorf("guest /proc TCP discovery: %w", err)
	}
	if err := checkTCP(ctx, c, func(ctx context.Context) (net.Conn, error) { return dial(ctx, socket) }); err != nil {
		return err
	}
	fmt.Println("guest exec, PTY, agent CLIs, device links, mounts, metadata, writable directories, orphan reaping and port discovery: OK")
	return nil
}

func checkTCP(ctx context.Context, c *agent.Client, dialAgent func(context.Context) (net.Conn, error)) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	stop := context.AfterFunc(ctx, func() { reader.CloseWithError(ctx.Err()) })
	defer stop()
	var stderr bytes.Buffer
	s, err := c.Exec(ctx, agent.ExecRequest{Argv: []string{"node", "-e", `const s=require('http').createServer((_,r)=>{r.end('tcp-smoke-ok');s.close()});s.listen(0,'127.0.0.1',()=>console.log(s.address().port))`}}, writer, &stderr)
	if err != nil {
		return err
	}
	// Close the pipe even if the server exits before it prints its port.
	done := make(chan struct{})
	var waitErr error
	go func() {
		code, err := s.Wait()
		if err == nil && code != 0 {
			err = fmt.Errorf("TCP smoke server exited %d", code)
		}
		writer.CloseWithError(err)
		waitErr = err
		close(done)
	}()
	defer func() { cancel(); reader.Close(); <-done }()
	line, err := bufio.NewReader(reader).ReadString('\n')
	if err != nil {
		return fmt.Errorf("start TCP smoke server: %w", err)
	}
	port, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		return err
	}
	conn, err := dialAgent(ctx)
	if err != nil {
		return err
	}
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	conn, err = agent.DialTCP(ctx, conn, port)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n"); err != nil {
		return err
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK || string(b) != "tcp-smoke-ok" {
		return fmt.Errorf("TCP agent forwarding: status=%d body=%q err=%v", resp.StatusCode, b, err)
	}
	<-done
	if waitErr != nil {
		return fmt.Errorf("TCP smoke server: %w: %s", waitErr, stderr.String())
	}
	fmt.Println("guest agent tcp feature and loopback HTTP forwarding: OK")
	return nil
}
