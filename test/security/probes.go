// Package securitytest runs the same adversarial probes against Linux backends.
package securitytest

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"boxpool.run/boxpool/internal/agent"
)

//go:embed namespaces.sh
var namespaceProbe string

//go:embed host-network.sh
var networkProbe string

// Run tests actual guest commands, never treats a missing tool as a denial,
// and verifies the host listeners receive no TCP or UDP traffic.
func Run(ctx context.Context, t *testing.T, c *agent.Client, backend string, gateways ...string) {
	t.Helper()
	tcp, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	udp, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	targets := slices.Clone(gateways)
	for _, a := range addrs {
		if ip, _, err := net.ParseCIDR(a.String()); err == nil && ip.To4() != nil {
			targets = append(targets, ip.String())
		}
	}
	slices.Sort(targets)
	targets = slices.Compact(targets)
	// Prove the wildcard listeners really answer on the host.
	control, err := net.DialTimeout("tcp4", tcp.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := tcp.Accept()
	if err != nil {
		t.Fatal(err)
	}
	control.Close()
	accepted.Close()
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "host-only-marker")
	if err := os.WriteFile(marker, []byte("host secret"), 0600); err != nil {
		t.Fatal(err)
	}
	run := func(script string, args ...string) {
		t.Helper()
		var out bytes.Buffer
		s, err := c.Exec(ctx, agent.ExecRequest{Argv: append([]string{"bash", "-eu", "-o", "pipefail", "-c", script, "probe"}, args...)}, &out, &out)
		if err != nil {
			t.Fatal(err)
		}
		code, err := s.Wait()
		if err != nil || code != 0 {
			t.Fatalf("security probe: exit %d, %v\n%s", code, err, out.String())
		}
		t.Log(out.String())
	}
	run(namespaceProbe, backend, strings.TrimSpace(string(boot)), marker)
	ports := []string{fmt.Sprint(tcp.Addr().(*net.TCPAddr).Port), fmt.Sprint(udp.LocalAddr().(*net.UDPAddr).Port)}
	run(networkProbe, append(ports, targets...)...)
	tcp.(*net.TCPListener).SetDeadline(time.Now().Add(200 * time.Millisecond))
	if conn, err := tcp.Accept(); err == nil {
		conn.Close()
		t.Error("guest reached the host TCP listener")
	} else if e, ok := err.(net.Error); !ok || !e.Timeout() {
		t.Fatal(err)
	}
	udp.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, _, err := udp.ReadFrom(make([]byte, 1024)); err == nil {
		t.Error("guest reached the host UDP listener")
	} else if e, ok := err.(net.Error); !ok || !e.Timeout() {
		t.Fatal(err)
	}
	// Legitimate name resolution and outbound TCP must still work.
	run(`timeout 20 bash -c 'exec 3<>/dev/tcp/example.com/80; printf "HEAD / HTTP/1.0\r\nHost: example.com\r\n\r\n" >&3; head -n1 <&3' | grep '^HTTP/'`)
}
