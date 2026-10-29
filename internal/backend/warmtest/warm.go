// Package warmtest shares the real-hypervisor warm-clone checks.
package warmtest

import (
	"bytes"
	"context"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"

	"boxpool.run/boxpool/internal/agent"
	"boxpool.run/boxpool/internal/backend"
)

type Driver interface {
	backend.Driver
	Warm(context.Context) error
	Dial(context.Context, string) (net.Conn, error)
}

// Check verifies independent disk/memory clones, identity, networking and
// persistence. prepared must reject a clone that would silently cold-boot.
func Check(t *testing.T, ctx context.Context, d Driver, network bool, prepared func(string) bool) {
	t.Helper()
	if err := d.Warm(ctx); err != nil {
		t.Fatal(err)
	}
	identities := map[string]bool{}
	addresses := map[string]bool{}
	ids := []string{"warm-one", "warm-two"}
	if runtime.NumCPU() < 2*backend.DefaultCPUs+1 {
		// Hosted KVM runners have four CPUs; keep the production host reserve.
		t.Log("one clone only: simultaneous identity/address check needs at least five CPUs")
		ids = ids[:1]
	}
	for _, id := range ids {
		start := time.Now()
		if _, err := d.Create(ctx, id, backend.Spec{Name: id}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := d.Remove(context.Background(), id); err != nil {
				t.Error(err)
			}
		})
		if !prepared(id) {
			t.Fatal("new sandbox has no warm saved state")
		}
		if err := d.Start(ctx, id); err != nil {
			t.Fatal(err)
		}
		c := agent.NewClient(func(ctx context.Context) (net.Conn, error) { return d.Dial(ctx, id) })
		t.Cleanup(c.CloseIdleConnections)
		run := func(script string) string {
			t.Helper()
			var out bytes.Buffer
			s, err := c.Exec(ctx, agent.ExecRequest{Argv: []string{"sh", "-eu", "-c", script}}, &out, &out)
			if err != nil {
				t.Fatal(err)
			}
			if code, err := s.Wait(); err != nil || code != 0 {
				t.Fatalf("%s: exit %d, %v: %s", script, code, err, out.String())
			}
			return strings.TrimSpace(out.String())
		}
		identity := run("cat /etc/machine-id")
		t.Logf("%s: create + restore + first exec: %v; machine-id %s", id, time.Since(start), identity)
		if len(identity) != 32 || identities[identity] {
			t.Fatalf("invalid or duplicate machine-id %q", identity)
		}
		identities[identity] = true
		if got := run("cat /proc/sys/kernel/hostname"); got != id {
			t.Fatalf("hostname %q, want %q", got, id)
		}
		run("test ! -e /root/clone-private; echo kept > /root/clone-private")
		checkNetwork := func() {
			t.Helper()
			address := run(`perl -MSocket -e 'socket(my $s, AF_INET, SOCK_DGRAM, 0) or die $!; connect($s, sockaddr_in(53, inet_aton("8.8.8.8"))) or die $!; print inet_ntoa((sockaddr_in(getsockname($s)))[1])'`)
			if addresses[address] && id == "warm-two" {
				t.Fatalf("duplicate address %q", address)
			}
			addresses[address] = true
			t.Logf("%s address %s; network: %s", id, address, run("cat /proc/net/pnp /proc/net/route /etc/resolv.conf"))
			run(`perl -MSocket -e 'gethostbyname("example.com") or die "DNS lookup failed"'`)
		}
		if network {
			checkNetwork()
		}
		if err := d.Pause(ctx, id); err != nil {
			t.Fatal(err)
		}
		if err := d.Resume(ctx, id); err != nil {
			t.Fatal(err)
		}
		if got := run("cat /etc/machine-id; cat /root/clone-private"); got != identity+"\nkept" {
			t.Fatalf("resume lost identity or disk: %q", got)
		}
		if network {
			run(`perl -MSocket -e 'gethostbyname("example.com") or die "DNS lookup failed"'`)
		}
		if err := d.Stop(ctx, id); err != nil {
			t.Fatal(err)
		}
		if err := d.Start(ctx, id); err != nil {
			t.Fatal(err)
		}
		if got := run("cat /etc/machine-id; cat /root/clone-private"); got != identity+"\nkept" {
			t.Fatalf("cold restart lost identity or disk: %q", got)
		}
	}
}
