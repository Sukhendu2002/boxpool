//go:build darwin && cgo && integration

package vz

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"boxpool.run/boxpool/internal/agent"
	"boxpool.run/boxpool/internal/artifacts"
	"boxpool.run/boxpool/internal/backend"
)

// TestIntegrationNetwork boots two sandboxes: A reaches the internet by
// name, and no packet from A reaches B. Needs internet access and a signed
// test binary.
func TestIntegrationNetwork(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	paths, err := artifacts.Fetch(ctx, filepath.Join(stateDir(t), artifacts.DirName))
	if err != nil {
		t.Fatal(err)
	}
	d, err := New(ctx, t.TempDir(), paths)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Error(err)
		}
	})
	exec := func(ctx context.Context, id, script string) (string, int, error) {
		c := agent.NewClient(func(ctx context.Context) (net.Conn, error) { return d.Dial(ctx, id) })
		defer c.CloseIdleConnections()
		var out bytes.Buffer
		s, err := c.Exec(ctx, agent.ExecRequest{Argv: []string{"bash", "-c", script}}, &out, &out)
		if err != nil {
			return "", 0, err
		}
		code, err := s.Wait()
		return out.String(), code, err
	}
	run := func(id, script string) string {
		t.Helper()
		out, code, err := exec(ctx, id, script)
		if err != nil || code != 0 {
			t.Fatalf("%s: %s: exit %d, %v: %s", id, script, code, err, out)
		}
		return out
	}
	for _, id := range []string{"net-a", "net-b"} {
		if _, err := d.Create(ctx, id, backend.Spec{CPUs: 1, MemoryMB: 512, DiskGB: 1}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := d.Remove(context.Background(), id); err != nil {
				t.Error(err)
			}
			if _, err := os.Stat(sandboxLayout(d.root, id).dir); !os.IsNotExist(err) {
				t.Errorf("Remove left %s: %v", id, err)
			}
		})
		if err := d.Start(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	// The image has no ip command; the kernel lists local addresses here.
	addr := func(id string) string {
		return strings.TrimSpace(run(id, `awk '/32 host LOCAL/ && prev !~ /^127\./ { print prev; exit } { prev = $2 }' /proc/net/fib_trie`))
	}
	a, b := addr("net-a"), addr("net-b")
	t.Logf("A %s, B %s; A's resolv.conf: %s", a, b, strings.TrimSpace(run("net-a", "cat /etc/resolv.conf")))

	// The image has no curl.
	out := run("net-a", `exec 3<>/dev/tcp/example.com/80 && printf 'HEAD / HTTP/1.0\r\nHost: example.com\r\n\r\n' >&3 && timeout 10 head -n 1 <&3`)
	if !strings.HasPrefix(out, "HTTP/1.") {
		t.Fatalf("HTTP from A: %q", out)
	}
	t.Logf("A to example.com:80: %s", strings.TrimSpace(out))

	listen, stopListen := context.WithCancel(ctx)
	defer stopListen()
	go exec(listen, "net-b", `exec perl -MIO::Socket::INET -e '$s = IO::Socket::INET->new(LocalPort => 8080, Listen => 5, ReuseAddr => 1) or die "$!"; while ($c = $s->accept) { print $c "hello from b\n"; close $c }'`)
	// The host can reach B, so the listener is up at B's address.
	var reply string
	for deadline := time.Now().Add(10 * time.Second); reply == "" && time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if c, err := net.DialTimeout("tcp", net.JoinHostPort(b, "8080"), time.Second); err == nil {
			c.SetReadDeadline(time.Now().Add(time.Second))
			buf := make([]byte, 64)
			n, _ := c.Read(buf)
			reply = string(buf[:n])
			c.Close()
		}
	}
	if reply != "hello from b\n" {
		t.Fatalf("host to B: %q", reply)
	}

	// B counts every TCP segment that reaches it, so A's SYNs must not.
	inSegs := `awk '/^Tcp:/ && n++ { print $11 }' /proc/net/snmp`
	before := run("net-b", inSegs)
	out, code, err := exec(ctx, "net-a", `timeout 5 bash -c 'exec 3<>/dev/tcp/`+b+`/8080 && cat <&3'`)
	if err != nil {
		t.Fatal(err)
	}
	if code == 0 {
		t.Fatalf("A reached B: %q", out)
	}
	if after := run("net-b", inSegs); after != before {
		t.Errorf("B received TCP segments while A tried to connect: InSegs %s -> %s", strings.TrimSpace(before), strings.TrimSpace(after))
	}
	t.Logf("A to B: exit %d: %s", code, strings.TrimSpace(out))
}
