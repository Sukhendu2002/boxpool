//go:build darwin && cgo && integration

package vz

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
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
)

// stateDir is fixed rather than t.TempDir, so artifacts are fetched once.
func stateDir(t *testing.T) string {
	if dir := os.Getenv("BOXPOOL_TEST_HOME"); dir != "" {
		return dir
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(cache, "boxpool-test")
}

// TestIntegration needs a test binary signed with the virtualization
// entitlement; see docs/development.md.
func TestIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	home := stateDir(t)
	paths, err := artifacts.Fetch(ctx, filepath.Join(home, artifacts.DirName))
	if err != nil {
		t.Fatal(err)
	}
	d, err := New(ctx, home, paths)
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 6)
	rand.Read(b)
	id := hex.EncodeToString(b)
	if _, err := d.Create(ctx, id, backend.Spec{Name: "it-" + id, CPUs: 1, MemoryMB: 512, DiskGB: 1}); err != nil {
		t.Fatal(err)
	}
	l := sandboxLayout(d.root, id)
	t.Cleanup(func() {
		if t.Failed() {
			b, _ := os.ReadFile(l.console)
			t.Logf("console tail:\n%s", b[max(0, len(b)-4096):])
		}
		if err := d.Remove(context.Background(), id); err != nil {
			t.Error(err)
		}
		if _, err := os.Stat(l.dir); !os.IsNotExist(err) {
			t.Errorf("Remove left %s: %v", l.dir, err)
		}
	})
	c := agent.NewClient(func(ctx context.Context) (net.Conn, error) { return d.Dial(ctx, id) })
	defer c.CloseIdleConnections()
	run := func(script string) string {
		t.Helper()
		var out bytes.Buffer
		s, err := c.Exec(ctx, agent.ExecRequest{Argv: []string{"sh", "-c", script}}, &out, &out)
		if err != nil {
			t.Fatal(err)
		}
		if code, err := s.Wait(); err != nil || code != 0 {
			t.Fatalf("%s: exit %d, %v: %s", script, code, err, out.String())
		}
		return out.String()
	}
	stop := func() {
		t.Helper()
		start := time.Now()
		if err := d.Stop(ctx, id); err != nil {
			t.Fatal(err)
		}
		t.Logf("Stop: %v", time.Since(start))
		// The guest kernel prints this when it powers off, not on a forced stop.
		if log, _ := os.ReadFile(l.console); !bytes.Contains(log, []byte("sysrq: Power Off")) {
			t.Errorf("the guest did not power off; console tail:\n%s", log[max(0, len(log)-4096):])
		}
		if d.running(id) != nil {
			t.Fatal("the VM still runs after Stop")
		}
		if _, err := d.Dial(ctx, id); err == nil {
			t.Error("Dial succeeded after Stop")
		}
	}

	for boot := 1; boot <= 3; boot++ {
		start := time.Now()
		if err := d.Start(ctx, id); err != nil {
			t.Fatal(err)
		}
		t.Logf("boot %d: Start until the agent answered /v1/version: %v", boot, time.Since(start))
		list, err := d.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, sb := range list {
			found = found || sb == backend.Sandbox{ID: id, Name: "it-" + id, State: backend.StateRunning}
		}
		if !found {
			t.Errorf("List does not show %s running: %+v", id, list)
		}
		if boot == 1 {
			if got := run("uname -m"); got != "aarch64\n" {
				t.Errorf("uname -m: %q, want aarch64", got)
			}
			// Linux reserves some of the configured 512 MiB for its kernel.
			out := run(`test "$(nproc)" -eq 1 && awk '/^MemTotal:/ { print; if ($2 < 419430 || $2 > 524288) exit 1 }' /proc/meminfo && test "$(blockdev --getsize64 /dev/vda)" -eq 1073741824 && df -k /`)
			t.Logf("requested 1 CPU, 512 MiB, 1 GiB disk:\n%s", out)
			// The file system, as well as its block device, must have grown.
			run(`df -k --output=size / | tail -n 1 | { read size; test "$size" -gt 900000; }`)
			if _, err := d.Create(ctx, "too-much-memory", backend.Spec{CPUs: 1, MemoryMB: 1 << 30, DiskGB: 1}); err == nil {
				t.Fatal("created a sandbox with 1 TiB of memory")
			}
			if _, err := os.Stat(sandboxLayout(d.root, "too-much-memory").dir); !os.IsNotExist(err) {
				t.Fatalf("refused request allocated disk: %v", err)
			}
			t.Logf("uname -a: %s", strings.TrimSpace(run("uname -a")))
			// No sync: Stop must flush it, or the next boot will not see it.
			run("echo kept > /root/stop-test")
			// A virtio console can emit multiple MiB between timer ticks.
			start := time.Now()
			run("head -c 4194304 /dev/zero | tr '\\0' x > /dev/hvc0")
			t.Logf("4 MiB to the virtio console: %v", time.Since(start))
			time.Sleep(100 * time.Millisecond) // let the pipe drain
			var total int64
			for _, p := range []string{l.console, l.console + ".1"} {
				if fi, err := os.Stat(p); err == nil {
					total += fi.Size()
				}
			}
			t.Logf("console.log and console.log.1: %d bytes", total)
			if total > 3<<20 {
				t.Errorf("console logs hold %d bytes after 4 MiB of output, want at most 3 MiB", total)
			}
		} else if got := run("cat /root/stop-test"); got != "kept\n" {
			t.Errorf("file written before Stop: %q, want kept", got)
		}
		if boot == 2 {
			// A process writing through an open file while Stop runs; it
			// stays in the background after the exec session ends.
			run("sh -c 'exec 3>>/root/busy; while :; do echo x >&3; done' >/dev/null 2>&1 &")
			run("sleep 0.5; test -s /root/busy")
		}
		if boot < 3 {
			stop()
		}
		if boot == 2 {
			fsck(t, l.rootfs)
		}
	}
	// Without fsfreeze, Stop still stops the VM but reports it as unclean.
	run("mv /usr/sbin/fsfreeze /root/")
	defer func(old time.Duration) { stopTimeout = old }(stopTimeout)
	stopTimeout = 3 * time.Second
	start := time.Now()
	if err := d.Stop(ctx, id); !errors.Is(err, backend.ErrUncleanStop) {
		t.Errorf("Stop without fsfreeze: %v, want ErrUncleanStop", err)
	} else {
		t.Logf("Stop without fsfreeze after %v: %v", time.Since(start), err)
	}
	if d.running(id) != nil {
		t.Error("the VM still runs after an unclean Stop")
	}

	// Close stops running VMs, as the daemon does on exit.
	if err := d.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	run("mv /root/fsfreeze /usr/sbin/")
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if d.running(id) != nil {
		t.Error("the VM still runs after Close")
	}
	// A boot that fails after VM creation must release the VM and its pipe
	// before returning, so a subsequent Start can reuse the same disk.
	oldTimeout := agentTimeout
	agentTimeout = time.Nanosecond
	err = d.Start(ctx, id)
	agentTimeout = oldTimeout
	if err == nil || d.running(id) != nil {
		t.Fatalf("failed boot left a VM running: %v", err)
	}
	if err := d.Start(ctx, id); err != nil {
		t.Fatalf("restart after failed boot: %v", err)
	}
	if err := d.Remove(ctx, id); err != nil {
		t.Fatalf("Remove of running VM: %v", err)
	}
}

// fsck checks that Stop left the sandbox's disk consistent, when the host
// has e2fsprogs (brew install e2fsprogs).
func fsck(t *testing.T, rootfs string) {
	t.Helper()
	e2fsck, err := exec.LookPath("e2fsck")
	if err != nil {
		e2fsck, err = exec.LookPath("/opt/homebrew/opt/e2fsprogs/sbin/e2fsck")
	}
	if err != nil {
		t.Logf("not checking the disk: %v", err)
		return
	}
	out, err := exec.Command(e2fsck, "-fn", rootfs).CombinedOutput()
	t.Logf("e2fsck -fn rootfs.ext4: %v\n%s", err, out)
	// e2fsck -n exits 0 even when the journal still needs replaying.
	if err != nil || bytes.Contains(out, []byte("journal recovery")) {
		t.Errorf("disk inconsistent after Stop with an active writer: %v", err)
	}
}
