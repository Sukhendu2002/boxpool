//go:build darwin && cgo && integration

package vz

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"boxpool.run/boxpool/internal/agent"
	"boxpool.run/boxpool/internal/artifacts"
	"boxpool.run/boxpool/internal/backend"
)

// netTest is a driver for the network tests, with helpers to run commands
// in its sandboxes.
type netTest struct {
	t   *testing.T
	ctx context.Context
	d   *Driver
}

func newNetTest(t *testing.T) *netTest {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
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
	return &netTest{t, ctx, d}
}

// start creates and starts a sandbox that the test removes at its end.
func (n *netTest) start(id string) {
	n.t.Helper()
	if _, err := n.d.Create(n.ctx, id, backend.Spec{CPUs: 1, MemoryMB: 512, DiskGB: 1}); err != nil {
		n.t.Fatal(err)
	}
	n.t.Cleanup(func() {
		if err := n.d.Remove(context.Background(), id); err != nil {
			n.t.Error(err)
		}
		if _, err := os.Stat(sandboxLayout(n.d.root, id).dir); !os.IsNotExist(err) {
			n.t.Errorf("Remove left %s: %v", id, err)
		}
	})
	if err := n.d.Start(n.ctx, id); err != nil {
		n.t.Fatal(err)
	}
}

func (n *netTest) exec(ctx context.Context, id, script string) (string, int, error) {
	c := agent.NewClient(func(ctx context.Context) (net.Conn, error) { return n.d.Dial(ctx, id) })
	defer c.CloseIdleConnections()
	var out bytes.Buffer
	s, err := c.Exec(ctx, agent.ExecRequest{Argv: []string{"bash", "-c", script}}, &out, &out)
	if err != nil {
		return "", 0, err
	}
	code, err := s.Wait()
	return out.String(), code, err
}

func (n *netTest) run(id, script string) string {
	n.t.Helper()
	out, code, err := n.exec(n.ctx, id, script)
	if err != nil || code != 0 {
		n.t.Fatalf("%s: %s: exit %d, %v: %s", id, script, code, err, out)
	}
	return out
}

// addr is the sandbox's address. The image has no ip command; the kernel
// lists local addresses in fib_trie.
func (n *netTest) addr(id string) string {
	n.t.Helper()
	return strings.TrimSpace(n.run(id, `awk '/32 host LOCAL/ && prev !~ /^127\./ { print prev; exit } { prev = $2 }' /proc/net/fib_trie`))
}

// listen serves reply on port 8080 of every address of the sandbox until ctx
// ends.
func (n *netTest) listen(ctx context.Context, id, reply string) {
	go n.exec(ctx, id, `exec perl -MIO::Socket::INET -e '$s = IO::Socket::INET->new(LocalPort => 8080, Listen => 5, ReuseAddr => 1) or die "$!"; while ($c = $s->accept) { print $c "`+reply+`\n"; close $c }'`)
}

// hostDial is what the Mac reads from addr:8080, retrying for wait.
func hostDial(addr string, wait time.Duration) string {
	for deadline := time.Now().Add(wait); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if c, err := net.DialTimeout("tcp", net.JoinHostPort(addr, "8080"), time.Second); err == nil {
			c.SetReadDeadline(time.Now().Add(time.Second))
			buf := make([]byte, 64)
			k, _ := c.Read(buf)
			c.Close()
			if k > 0 {
				return string(buf[:k])
			}
		}
	}
	return ""
}

// subnet is the /24 holding an IPv4 address.
func subnet(addr string) string { return addr[:strings.LastIndex(addr, ".")] }

// addRoute adds a host route to $1 via the gateway $2 with SIOCADDRT (the
// image has no ip or route command). The struct is arm64's rtentry.
const addRoute = `perl -MSocket -e '
sub sa { pack("S n a4 x8", AF_INET, 0, inet_aton($_[0])) }
socket(my $s, PF_INET, SOCK_DGRAM, 0) or die "socket: $!\n";
ioctl($s, 0x890B, pack("Q a16 a16 a16 S x6 Q Q s x6 Q Q Q S x6", 0, sa($ARGV[0]), sa($ARGV[1]), sa("255.255.255.255"), 7, 0, 0, 0, 0, 0, 0, 0)) or die "SIOCADDRT: $!\n"' `

// addAddress adds $1 to eth0 as the alias eth0:1 with SIOCSIFADDR.
const addAddress = `perl -MSocket -e '
socket(my $s, PF_INET, SOCK_DGRAM, 0) or die "socket: $!\n";
ioctl($s, 0x8916, pack("a16 S n a4 x8 x8", "eth0:1", AF_INET, 0, inet_aton($ARGV[0]))) or die "SIOCSIFADDR: $!\n"' `

// inSegs prints how many TCP segments have reached the sandbox.
const inSegs = `awk '/^Tcp:/ && n++ { print $11 }' /proc/net/snmp`

// TestIntegrationNetwork boots sandboxes A and B: A reaches the internet by
// name and no packet from A reaches B, even routed through A's gateway.
// Then A stops and C starts: C gets a network of its own and cannot take
// over A's address, and A keeps its address when it starts again. Needs
// internet access and a signed test binary.
func TestIntegrationNetwork(t *testing.T) {
	version, err := unix.Sysctl("kern.osproductversion")
	if err != nil {
		t.Fatal(err)
	}
	if major, _ := strconv.Atoi(strings.Split(version, ".")[0]); major < 26 {
		t.Skipf("macOS %s has no isolated networks; TestIntegrationNoNetwork covers it", version)
	}
	if !IsolatedNetworks() {
		t.Errorf("macOS %s, but isolated networks are off: built with an SDK older than macOS 26's?", version)
	}
	n := newNetTest(t)
	n.start("net-a")
	n.start("net-b")
	a, b := n.addr("net-a"), n.addr("net-b")
	t.Logf("A %s, B %s; A's resolv.conf: %s", a, b, strings.TrimSpace(n.run("net-a", "cat /etc/resolv.conf")))
	if subnet(a) == subnet(b) {
		t.Errorf("A %s and B %s share a network", a, b)
	}

	// The image has no curl.
	out := n.run("net-a", `exec 3<>/dev/tcp/example.com/80 && printf 'HEAD / HTTP/1.0\r\nHost: example.com\r\n\r\n' >&3 && timeout 10 head -n 1 <&3`)
	if !strings.HasPrefix(out, "HTTP/1.") {
		t.Fatalf("HTTP from A: %q", out)
	}
	t.Logf("A to example.com:80: %s", strings.TrimSpace(out))

	listen, stopListen := context.WithCancel(n.ctx)
	defer stopListen()
	n.listen(listen, "net-b", "hello from b")
	// The host can reach B, so the listener is up at B's address.
	if reply := hostDial(b, 10*time.Second); reply != "hello from b\n" {
		t.Fatalf("host to B: %q", reply)
	}

	// B counts every TCP segment that reaches it, so A's SYNs must not.
	probe := func(how string) {
		t.Helper()
		// The host's connections to B may still be closing.
		before := n.run("net-b", inSegs)
		for settled := ""; settled != before; {
			time.Sleep(time.Second)
			settled, before = before, n.run("net-b", inSegs)
		}
		out, code, err := n.exec(n.ctx, "net-a", `timeout 5 bash -c 'exec 3<>/dev/tcp/`+b+`/8080 && cat <&3'`)
		if err != nil {
			t.Fatal(err)
		}
		if code == 0 {
			t.Errorf("A reached B %s: %q", how, out)
		}
		if after := n.run("net-b", inSegs); after != before {
			t.Errorf("B received TCP segments while A tried to connect %s: InSegs %s -> %s", how, strings.TrimSpace(before), strings.TrimSpace(after))
		}
		t.Logf("A to B %s: exit %d, B's InSegs steady at %s: %s", how, code, strings.TrimSpace(before), strings.TrimSpace(out))
	}
	probe("directly")
	// Root in A routes B's address through A's gateway explicitly, which
	// delivered A's packets to B on the framework's shared NAT.
	gw := strings.TrimSpace(n.run("net-a", `awk '$2 == "00000000" { print $3 }' /proc/net/route`))
	le, err := strconv.ParseUint(gw, 16, 32)
	if err != nil {
		t.Fatalf("A's gateway %q: %v", gw, err)
	}
	gateway := net.IP(binary.LittleEndian.AppendUint32(nil, uint32(le))).String()
	n.run("net-a", addRoute+b+" "+gateway)
	t.Logf("A's routes: %s", n.run("net-a", "cat /proc/net/route"))
	probe("through its gateway " + gateway)

	// A stopped sandbox keeps its network: C must get another one, so it
	// cannot take A's address there and receive traffic meant for A.
	if err := n.d.Stop(n.ctx, "net-a"); err != nil {
		t.Fatal(err)
	}
	n.start("net-c")
	c := n.addr("net-c")
	t.Logf("A stopped; C %s", c)
	if subnet(c) == subnet(a) {
		t.Errorf("C %s got stopped A's network (A %s)", c, a)
	}
	if v, ok := n.d.networks.Load("net-a"); ok {
		if w, _ := n.d.networks.Load("net-c"); w.(unsafe.Pointer) == v.(unsafe.Pointer) {
			t.Error("C got stopped A's vmnet network")
		}
	}
	n.run("net-c", addAddress+a)
	t.Logf("C's addresses: %s", strings.Fields(n.run("net-c", `awk '/32 host LOCAL/ { print prev } { prev = $2 }' /proc/net/fib_trie | sort -u`)))
	n.listen(listen, "net-c", "hello from c")
	if reply := hostDial(c, 10*time.Second); reply != "hello from c\n" {
		t.Fatalf("host to C: %q", reply)
	}
	if reply := hostDial(a, 3*time.Second); reply != "" {
		t.Errorf("host to stopped A's address %s: %q", a, reply)
	}

	if err := n.d.Start(n.ctx, "net-a"); err != nil {
		t.Fatal(err)
	}
	if again := n.addr("net-a"); again != a {
		t.Errorf("A's address changed across Stop and Start: %s -> %s", a, again)
	}
}

// TestIntegrationNoNetwork checks that without isolated networks a sandbox
// starts with no network device rather than on the framework's shared NAT.
func TestIntegrationNoNetwork(t *testing.T) {
	n := newNetTest(t)
	n.d.isolated = false // as before macOS 26
	start := time.Now()
	n.start("nonet")
	t.Logf("started in %s", time.Since(start).Round(time.Millisecond))
	if got := strings.Fields(n.run("nonet", "ls /sys/class/net")); len(got) != 1 || got[0] != "lo" {
		t.Errorf("network interfaces: %q, want only lo", got)
	}
	if _, ok := n.d.networks.Load("nonet"); ok {
		t.Error("the sandbox holds a vmnet network")
	}
}

// TestIntegrationNetworkLimit takes every network the process can hold:
// Start then fails with a clear message, also while a removed sandbox's
// network is in quarantine, and works once the quarantine is over.
func TestIntegrationNetworkLimit(t *testing.T) {
	if !IsolatedNetworks() {
		t.Skip("no isolated networks")
	}
	n := newNetTest(t)
	defer func(q time.Duration) { quarantine = q }(quarantine)
	quarantine = 500 * time.Millisecond
	var held []unsafe.Pointer
	for {
		nw, err := takeNetwork()
		if errors.Is(err, errTooManyNetworks) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, nw)
	}
	defer func() {
		for _, nw := range held {
			putNetwork(nw)
		}
		// Later tests in this process need the networks back at once; no
		// live sandbox's traffic can reach them.
		networks.Lock()
		for i := range networks.idle {
			networks.idle[i].removed = time.Time{}
		}
		networks.Unlock()
	}()
	if _, err := n.d.Create(n.ctx, "limit", backend.Spec{CPUs: 1, MemoryMB: 512, DiskGB: 1}); err != nil {
		t.Fatal(err)
	}
	defer n.d.Remove(context.Background(), "limit")
	if err := n.d.Start(n.ctx, "limit"); !errors.Is(err, errTooManyNetworks) {
		t.Fatalf("Start with %d networks held: %v", maxNetworks, err)
	} else {
		t.Log(err)
	}
	removed := held[len(held)-1]
	held = held[:len(held)-1]
	putNetwork(removed)
	if err := n.d.Start(n.ctx, "limit"); !errors.Is(err, errTooManyNetworks) {
		t.Fatalf("Start with the only free network in quarantine: %v", err)
	}
	time.Sleep(quarantine)
	if err := n.d.Start(n.ctx, "limit"); err != nil {
		t.Fatal(err)
	}
	if v, _ := n.d.networks.Load("limit"); v.(unsafe.Pointer) != removed {
		t.Error("Start did not take the network whose quarantine was over")
	}
}
