//go:build linux && integration

package firecracker

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"boxpool.run/boxpool/internal/backend"
	"boxpool.run/boxpool/internal/store"
)

// Each regression re-executes in its own disposable network namespace.
// Even a root invocation of the entire test suite cannot mutate host links.
func networkNamespace(t *testing.T) bool {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root and unshare for an isolated network namespace")
	}
	if os.Getenv("BOXPOOL_NETNS_TEST") == "1" {
		current, err := os.Readlink("/proc/self/ns/net")
		if err != nil {
			t.Fatal(err)
		}
		initial, err := os.Readlink("/proc/1/ns/net")
		if err != nil || current == initial {
			t.Fatal("refusing host network namespace")
		}
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "unshare", "-n", os.Args[0], "-test.v", "-test.run=^"+regexp.QuoteMeta(t.Name())+"$", "-test.timeout=20s")
	cmd.Env = append(cmd.Environ(), "BOXPOOL_NETNS_TEST=1")
	out, err := cmd.CombinedOutput()
	t.Logf("namespace regression:\n%s", out)
	if err != nil {
		t.Fatal(err)
	}
	return false
}

func namespaceRun(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := run(context.Background(), "", name, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func namespaceDriver(t *testing.T) *Driver {
	t.Helper()
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	namespaceRun(t, "ip", "link", "add", "test-uplink", "type", "dummy")
	namespaceRun(t, "ip", "addr", "add", "192.0.2.1/24", "dev", "test-uplink")
	namespaceRun(t, "ip", "link", "set", "test-uplink", "up")
	namespaceRun(t, "ip", "route", "add", "default", "dev", "test-uplink")
	return &Driver{root: root, store: st, firecracker: "/nonexistent-test-firecracker"}
}

func namespaceSpec(t *testing.T, d *Driver, id string) {
	t.Helper()
	l := sandboxLayout(d.root, id)
	if err := os.Mkdir(l.dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.spec, []byte(`{"CPUs":1,"MemoryMB":256}`), 0600); err != nil {
		t.Fatal(err)
	}
}

func requireNoOwnedNetwork(t *testing.T, id string) {
	t.Helper()
	if _, err := net.InterfaceByName(tapName(id)); err == nil {
		t.Error("owned TAP remains")
	}
	if _, err := run(context.Background(), "", "nft", "list", "table", "inet", "boxpool"); err == nil {
		t.Error("boxpool table remains")
	}
	if out := namespaceRun(t, "iptables", "-S"); strings.Contains(out, "--comment boxpool") {
		t.Errorf("own accepts remain: %s", out)
	}
}

func TestNetworkOwnership(t *testing.T) {
	if !networkNamespace(t) {
		return
	}
	d := namespaceDriver(t)
	ctx := context.Background()
	namespaceRun(t, "ip", "link", "add", "bp-unrelated", "type", "dummy")
	namespaceRun(t, "ip", "tuntap", "add", "dev", "bp-foreign-tap", "mode", "tap")
	if err := d.sweepNetwork(ctx); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"bp-unrelated", "bp-foreign-tap"} {
		if _, err := net.InterfaceByName(name); err != nil {
			t.Fatalf("foreign %s deleted: %v", name, err)
		}
	}
	for _, tc := range []struct {
		name, kind string
		alias      bool
	}{
		{"dummy", "dummy", false},
		{"dummy-with-alias", "dummy", true},
		{"unmarked-tap", "tap", false},
		{"tun-with-alias", "tun", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := tc.name
			namespaceSpec(t, d, id)
			slot, err := d.store.NetSlot(ctx, id, netSlots)
			if err != nil {
				t.Fatal(err)
			}
			name := tapName(id)
			if tc.kind == "dummy" {
				namespaceRun(t, "ip", "link", "add", name, "type", "dummy")
			} else {
				namespaceRun(t, "ip", "tuntap", "add", "dev", name, "mode", tc.kind)
			}
			if tc.alias {
				namespaceRun(t, "ip", "link", "set", "dev", name, "alias", d.tapAlias(id, slot))
			}
			if err := d.Start(ctx, id); err == nil || !strings.Contains(err.Error(), "foreign interface") {
				t.Fatalf("Start collision: %v", err)
			}
			if err := d.Stop(ctx, id); err == nil {
				t.Error("Stop must report foreign collision")
			}
			if err := d.Remove(ctx, id); err == nil {
				t.Error("Remove must report foreign collision")
			}
			if err := d.sweepNetwork(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := net.InterfaceByName(name); err != nil {
				t.Fatal("foreign collision deleted")
			}
			namespaceRun(t, "ip", "link", "del", "dev", name)
			if err := d.Remove(ctx, id); err != nil {
				t.Fatal(err)
			}
		})
	}
	// Both firewall cleanup and allocation cleanup ignore unrelated prefixes.
	const orphan = "owned-orphan"
	if _, err := d.startNetwork(ctx, orphan); err != nil {
		t.Fatal(err)
	}
	if err := d.sweepNetwork(ctx); err != nil {
		t.Fatal(err)
	}
	requireNoOwnedNetwork(t, orphan)
	slots, err := d.store.NetSlots(ctx)
	if err != nil || len(slots) != 0 {
		t.Fatalf("orphan allocations: %v, %v", slots, err)
	}
	for _, name := range []string{"bp-unrelated", "bp-foreign-tap"} {
		if _, err := net.InterfaceByName(name); err != nil {
			t.Fatal("foreign prefix device deleted")
		}
	}
	// A crash after persistence but before address assignment is recoverable.
	const partial = "persisted-partial"
	slot, err := d.store.NetSlot(ctx, partial, netSlots)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.createTap(ctx, partial, slot); err != nil {
		t.Fatal(err)
	}
	if err := d.sweepNetwork(ctx); err != nil {
		t.Fatal(err)
	}
	requireNoOwnedNetwork(t, partial)
	// Missing allocation is never enough authority to delete a marked TAP.
	const unknown = "unallocated"
	if err := d.createTap(ctx, unknown, 0); err != nil {
		t.Fatal(err)
	}
	if err := d.stopNetwork(ctx, unknown); err != nil {
		t.Fatal(err)
	}
	if err := d.sweepNetwork(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := net.InterfaceByName(tapName(unknown)); err != nil {
		t.Fatal("unallocated TAP deleted")
	}
	namespaceRun(t, "ip", "link", "del", "dev", tapName(unknown))
}

func TestNetworkCanceledCleanup(t *testing.T) {
	if !networkNamespace(t) {
		return
	}
	d := namespaceDriver(t)
	namespaceRun(t, "iptables", "-P", "FORWARD", "DROP")
	namespaceRun(t, "iptables", "-A", "FORWARD", "-s", "192.0.2.99", "-j", "ACCEPT")
	baseline := namespaceRun(t, "iptables", "-S")
	for _, op := range []string{"Stop", "Remove"} {
		t.Run(op, func(t *testing.T) {
			id := strings.ToLower(op)
			namespaceSpec(t, d, id)
			if _, err := d.startNetwork(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			// Include stale runtime files so caller cancellation also
			// exercises removal after the process is already gone.
			l := sandboxLayout(d.root, id)
			for _, path := range []string{l.pid, l.api, l.vsock} {
				if err := os.WriteFile(path, []byte("not-a-live-pid"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			var err error
			if op == "Stop" {
				err = d.Stop(ctx, id)
			} else {
				err = d.Remove(ctx, id)
			}
			if err != nil {
				t.Fatal(err)
			}
			requireNoOwnedNetwork(t, id)
			for _, path := range []string{l.pid, l.api, l.vsock} {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("runtime file remains: %s", path)
				}
			}
			if got := namespaceRun(t, "iptables", "-S"); got != baseline {
				t.Fatalf("unrelated rules changed: %s", got)
			}
			if op == "Remove" {
				slots, err := d.store.NetSlots(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if _, ok := slots[id]; ok {
					t.Error("Remove retained allocation")
				}
				if _, err := os.Stat(sandboxLayout(d.root, id).dir); !errors.Is(err, os.ErrNotExist) {
					t.Error("Remove retained directory")
				}
			} else if err := d.Remove(ctx, id); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func namespaceIPStub(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ip"), []byte("#!/bin/sh\n"+script+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
}

func TestNetworkCleanupErrors(t *testing.T) {
	if !networkNamespace(t) {
		return
	}
	d := namespaceDriver(t)
	const id = "cleanup-error"
	namespaceSpec(t, d, id)
	if _, err := d.startNetwork(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	t.Run("deletion failure", func(t *testing.T) {
		ip, err := exec.LookPath("ip")
		if err != nil {
			t.Fatal(err)
		}
		namespaceIPStub(t, `if [ "$1 $2" = "link del" ]; then echo deliberate-delete-failure >&2; exit 42; fi`+"\nexec "+ip+` "$@"`)
		for _, op := range []func(context.Context, string) error{d.Stop, d.Remove} {
			if err := op(context.Background(), id); err == nil || !strings.Contains(err.Error(), "deliberate-delete-failure") {
				t.Fatalf("cleanup error lost: %v", err)
			}
		}
		if _, err := net.InterfaceByName(tapName(id)); err != nil {
			t.Fatal("TAP unexpectedly gone")
		}
		if _, err := os.Stat(sandboxLayout(d.root, id).dir); err != nil {
			t.Fatal("directory removed after cleanup failure")
		}
	})
	t.Run("bounded after cancellation", func(t *testing.T) {
		namespaceIPStub(t, "exec sleep 30")
		old := stopTimeout
		stopTimeout = 150 * time.Millisecond
		defer func() { stopTimeout = old }()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		started := time.Now()
		if err := d.Stop(ctx, id); err == nil {
			t.Error("missing timeout error")
		}
		if elapsed := time.Since(started); elapsed > 2*time.Second {
			t.Fatalf("cleanup unbounded: %s", elapsed)
		}
	})
	if err := d.Remove(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	requireNoOwnedNetwork(t, id)
}

func TestNetworkFailedStart(t *testing.T) {
	if !networkNamespace(t) {
		return
	}
	d := namespaceDriver(t)
	const id = "failed-start"
	namespaceSpec(t, d, id)
	if err := d.Start(context.Background(), id); err == nil {
		t.Fatal("wanted executable failure")
	}
	requireNoOwnedNetwork(t, id)
	if err := d.Remove(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	slots, err := d.store.NetSlots(context.Background())
	if err != nil || len(slots) != 0 {
		t.Fatalf("allocations: %v %v", slots, err)
	}
}

func TestNetworkPartialTap(t *testing.T) {
	if !networkNamespace(t) {
		return
	}
	d := namespaceDriver(t)
	const id = "partial-tap"
	t.Run("alias failure", func(t *testing.T) {
		namespaceIPStub(t, "exit 42")
		if err := d.createTap(context.Background(), id, 0); err == nil {
			t.Fatal("wanted alias failure")
		}
		if _, err := net.InterfaceByName(tapName(id)); err == nil {
			t.Fatal("nonpersistent TAP leaked")
		}
	})
	t.Run("creator crash", func(t *testing.T) {
		ip, err := exec.LookPath("ip")
		if err != nil {
			t.Fatal(err)
		}
		// The alias is installed, then the creating Go process is killed before
		// TUNSETPERSIST. Its fd closes and the nonpersistent device disappears.
		namespaceIPStub(t, ip+` "$@" || exit`+"\n"+`kill -KILL "$PPID"`)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestNetworkTapCrashHelper$", "-test.timeout=2s")
		cmd.Env = append(cmd.Environ(), "BOXPOOL_TAP_CRASH_ROOT="+d.root)
		if out, err := cmd.CombinedOutput(); err == nil {
			t.Fatalf("creator survived: %s", out)
		}
		if _, err := net.InterfaceByName(tapName(id)); err == nil {
			t.Fatal("crashed creator leaked TAP")
		}
	})
}

func TestNetworkTapCrashHelper(t *testing.T) {
	root := os.Getenv("BOXPOOL_TAP_CRASH_ROOT")
	if root == "" {
		t.Skip("only run as partial TAP subprocess")
	}
	if !networkNamespace(t) {
		return
	}
	d := &Driver{root: root}
	if err := d.createTap(context.Background(), "partial-tap", 0); err != nil {
		t.Fatal(err)
	}
	t.Fatal("creator should have been killed before persistence")
}

func TestNetworkCanceledRunningStop(t *testing.T) {
	if !networkNamespace(t) {
		return
	}
	d := namespaceDriver(t)
	const id = "running-stop"
	namespaceSpec(t, d, id)
	if _, err := d.startNetwork(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	l := sandboxLayout(d.root, id)
	// argv[0] supplies the identity checked by running; sleep is a disposable
	// stand-in for a VM that cannot answer the shutdown endpoint.
	cmd := exec.Command("sleep", "30")
	cmd.Args[0] = l.api
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	if err := os.WriteFile(l.pid, []byte(strconv.Itoa(cmd.Process.Pid)), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := d.Stop(ctx, id); !errors.Is(err, backend.ErrUncleanStop) {
		t.Fatalf("Stop: %v", err)
	}
	if _, ok := running(l); ok {
		t.Fatal("process still runs")
	}
	requireNoOwnedNetwork(t, id)
	if err := d.Remove(ctx, id); err != nil {
		t.Fatal(err)
	}
}
