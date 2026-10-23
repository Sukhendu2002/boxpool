// Command smoke checks the built guest over Firecracker's host-side vsock UDS.
// It is deliberately image-specific, not a backend driver or test framework.
package main

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"net"
	"os"
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
	fmt.Println("guest exec, PTY, device links, mounts, metadata, writable directories, orphan reaping and port discovery: OK")
	return nil
}
