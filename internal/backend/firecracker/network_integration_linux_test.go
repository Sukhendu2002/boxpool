//go:build linux && integration

package firecracker

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"boxpool.run/boxpool/internal/agent"
	"boxpool.run/boxpool/internal/artifacts"
	"boxpool.run/boxpool/internal/backend"
	"boxpool.run/boxpool/internal/store"
)

// TestIntegrationNetwork boots two networked sandboxes, A and B. It needs
// root. With BOXPOOL_TEST_APT=1 it also installs curl and ping in A from the
// image's Debian mirror and uses them.
func TestIntegrationNetwork(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root for tap devices and firewall rules")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	home := stateDir(t)
	paths, err := artifacts.Fetch(ctx, filepath.Join(home, artifacts.DirName))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(home, "network-state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() }) // after the Removes, which run first
	d, err := New(ctx, home, paths, st)
	if err != nil {
		t.Fatal(err)
	}

	var ids []string
	t.Cleanup(func() {
		for _, id := range ids {
			if t.Failed() {
				b, _ := os.ReadFile(sandboxLayout(d.root, id).console)
				t.Logf("%s console:\n%s", id, b)
			}
			if err := d.Remove(context.Background(), id); err != nil {
				t.Error(err)
			}
		}
		assertNoNetworkLeft(t)
	})
	nics := map[string]nic{}
	for range 2 {
		b := make([]byte, 6)
		rand.Read(b)
		id := hex.EncodeToString(b)
		if _, err := d.Create(ctx, id, backend.Spec{Name: "it-" + id, CPUs: 1, MemoryMB: 256}); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		if err := d.Start(ctx, id); err != nil {
			t.Fatal(err)
		}
		slot, err := st.NetSlot(ctx, id, netSlots)
		if err != nil {
			t.Fatal(err)
		}
		nics[id] = sandboxNIC(id, slot)
	}
	a, b := ids[0], ids[1]
	t.Logf("A %s: %s on %s; B %s: %s on %s", a, nics[a].guest, nics[a].tap, b, nics[b].guest, nics[b].tap)
	sh := func(id, script string) (string, int) {
		t.Helper()
		c := agent.NewClient(func(ctx context.Context) (net.Conn, error) { return d.Dial(ctx, id) })
		defer c.CloseIdleConnections()
		var out bytes.Buffer
		s, err := c.Exec(ctx, agent.ExecRequest{Argv: []string{"bash", "-o", "pipefail", "-c", script}}, &out, &out)
		if err != nil {
			t.Fatal(err)
		}
		code, err := s.Wait()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(out.String()), code
	}
	must := func(id, script string) string {
		t.Helper()
		out, code := sh(id, script)
		if code != 0 {
			t.Fatalf("%s: exit %d: %s", script, code, out)
		}
		return out
	}
	// connect exits 0 when a TCP connection to addr opens within 3 seconds.
	connect := func(addr hostPort) string {
		return fmt.Sprintf("timeout 3 bash -c 'exec 3<>/dev/tcp/%s/%d'", addr.ip, addr.port)
	}
	blocked := func(id, what string, addr hostPort) {
		t.Helper()
		if out, code := sh(id, connect(addr)); code == 0 {
			t.Errorf("%s connected to %s (%s:%d): %s", id, what, addr.ip, addr.port, out)
		} else {
			t.Logf("%s to %s (%s:%d): blocked (exit %d)", id, what, addr.ip, addr.port, code)
		}
	}

	rules, _ := exec.Command("nft", "list", "table", "inet", "boxpool").CombinedOutput()
	accepts, _ := exec.Command("sh", "-c", "iptables -S | grep boxpool").CombinedOutput()
	t.Logf("nft list table inet boxpool:\n%s\niptables rules:\n%s", rules, accepts)
	t.Logf("A resolv.conf: %q", must(a, "cat /etc/resolv.conf"))
	t.Logf("A getent hosts example.com: %s", must(a, "getent hosts example.com"))
	head := must(a, `exec 3<>/dev/tcp/example.com/80 && printf 'HEAD / HTTP/1.0\r\nHost: example.com\r\n\r\n' >&3 && head -n1 <&3`)
	t.Logf("A HTTP HEAD http://example.com: %s", head)
	if !strings.HasPrefix(head, "HTTP/") {
		t.Errorf("HTTP from A: %q", head)
	}

	// B listens on 8080 and reaches itself, so A's failure is the firewall.
	must(b, `perl -MIO::Socket::INET -e '$s = IO::Socket::INET->new(LocalPort => 8080, Listen => 5, ReuseAddr => 1) or die $!; while ($c = $s->accept) { print $c "b\n"; close $c }' >/dev/null 2>&1 &`)
	bAddr := hostPort{nics[b].guest.String(), 8080}
	if out, code := sh(b, "sleep 0.5; "+connect(bAddr)); code != 0 {
		t.Fatalf("B cannot reach its own listener: exit %d: %s", code, out)
	}
	blocked(a, "B's listener", bAddr)

	// Host services: one on loopback only and one on every address.
	lo := listen(t, "127.0.0.1:0")
	all := listen(t, "0.0.0.0:0")
	hostIP := outboundIP(t)
	// Prove both host listeners reachable locally before attributing the
	// guest's connection failures to isolation.
	for _, addr := range []hostPort{{"127.0.0.1", lo}, {hostIP, all}} {
		c, err := net.DialTimeout("tcp", net.JoinHostPort(addr.ip, fmt.Sprint(addr.port)), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		c.Close()
	}
	blocked(a, "a host service on 127.0.0.1, through A's gateway", hostPort{nics[a].gateway.String(), lo})
	blocked(a, "a host service on 0.0.0.0, through A's gateway", hostPort{nics[a].gateway.String(), all})
	blocked(a, "a host service on 0.0.0.0, through B's gateway", hostPort{nics[b].gateway.String(), all})
	blocked(a, "a host service on 0.0.0.0, at the host's LAN IP", hostPort{hostIP, all})
	if os.Getenv("BOXPOOL_TEST_APT") == "1" {
		must(a, "apt-get -qq update && DEBIAN_FRONTEND=noninteractive apt-get -qq install -y --no-install-recommends curl iputils-ping >/dev/null")
		probe := func(p networkProbe) string {
			t.Helper()
			out, code := sh(a, p.command)
			if err := p.validate(out, code); err != nil {
				t.Fatal(err)
			}
			return out
		}
		t.Logf("A HTTPS: %s", probe(httpsProbe))
		for _, target := range []string{nics[b].guest.String(), nics[b].gateway.String(), nics[a].gateway.String(), hostIP} {
			if out, code := sh(a, "ping -c1 -W2 "+target); code == 0 {
				t.Errorf("A pinged %s: %s", target, out)
			} else {
				t.Logf("A ping %s: fails (exit %d)", target, code)
			}
		}
		t.Logf("A external ping: %s", probe(pingProbe))
	}
}

type hostPort struct {
	ip   string
	port int
}

// listen accepts TCP connections on addr until the test ends and returns
// its port.
func listen(t *testing.T, addr string) int {
	t.Helper()
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			t.Logf("host listener %s accepted a connection from %s", l.Addr(), c.RemoteAddr())
			c.Close()
		}
	}()
	return l.Addr().(*net.TCPAddr).Port
}

// outboundIP is the host's address on its outbound interface.
func outboundIP(t *testing.T) string {
	t.Helper()
	c, err := net.Dial("udp", "1.1.1.1:53")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).IP.String()
}

// assertNoNetworkLeft checks that no tap device or firewall rule of boxpool's
// is left.
func assertNoNetworkLeft(t *testing.T) {
	t.Helper()
	ifaces, _ := net.Interfaces()
	for _, i := range ifaces {
		if strings.HasPrefix(i.Name, tapPrefix) {
			t.Errorf("tap device %s left behind", i.Name)
		}
	}
	if out, err := exec.Command("nft", "list", "table", "inet", "boxpool").CombinedOutput(); err == nil {
		t.Errorf("nftables table left behind:\n%s", out)
	}
	if out, err := exec.Command("iptables", "-S").CombinedOutput(); err == nil && bytes.Contains(out, []byte("boxpool")) {
		t.Errorf("iptables rules left behind:\n%s", out)
	}
}
