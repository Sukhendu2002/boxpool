//go:build linux && integration

package firecracker

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"boxpool.run/boxpool/internal/agent"
	"boxpool.run/boxpool/internal/artifacts"
	"boxpool.run/boxpool/internal/backend"
)

// stateDir is fixed rather than t.TempDir: artifacts are fetched once, and a
// run killed half-way leaves an orphan for the next run's New to kill.
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

func TestIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	d, id, l, run := newSandbox(t, ctx, backend.Spec{CPUs: 1, MemoryMB: 512, DiskGB: 1})
	stop := func() {
		t.Helper()
		start := time.Now()
		if err := d.Stop(ctx, id); err != nil {
			t.Fatal(err)
		}
		t.Logf("Stop: %v", time.Since(start))
		// Firecracker logs this only when the guest ended the VM, not on a signal.
		if log, _ := os.ReadFile(l.log); !bytes.Contains(log, []byte("exiting successfully")) {
			t.Errorf("firecracker did not exit cleanly; its log:\n%s", log)
		}
		if _, ok := running(l); ok {
			t.Fatal("firecracker still runs after Stop")
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
			// nproc follows the virtual CPU count. Linux reserves some of
			// the configured 512 MiB for its kernel, so MemTotal is lower.
			out := run(`test "$(nproc)" -eq 1 && awk '/^MemTotal:/ { print; if ($2 < 419430 || $2 > 524288) exit 1 }' /proc/meminfo && test "$(blockdev --getsize64 /dev/vda)" -eq 1073741824 && df -k /`)
			t.Logf("requested 1 CPU, 512 MiB, 1 GiB disk:\n%s", out)
			// The filesystem, as well as its block device, must have grown.
			run(`df -k --output=size / | tail -n 1 | { read size; test "$size" -gt 900000; }`)
			if _, err := d.Create(ctx, "too-much-memory", backend.Spec{CPUs: 1, MemoryMB: 1 << 30, DiskGB: 1}); err == nil || !strings.Contains(err.Error(), "insufficient memory") {
				t.Fatalf("capacity refusal: %v", err)
			}
			if _, err := os.Stat(sandboxLayout(d.root, "too-much-memory").dir); !os.IsNotExist(err) {
				t.Fatalf("refused request allocated disk: %v", err)
			}
			out = run("uname -a")
			t.Logf("uname -a: %s", strings.TrimSpace(out))
			if !strings.HasPrefix(out, "Linux ") {
				t.Errorf("uname -a: %q", out)
			}
			t.Logf("fsfreeze: %s", strings.TrimSpace(run("command -v fsfreeze")))
			// No sync: Stop must flush it, or the next boot will not see it.
			run("echo kept > /root/stop-test")
			// More than the console's bound of about 2 MiB plus a second's output.
			start := time.Now()
			run("head -c 4194304 /dev/zero | tr '\\0' x > /dev/ttyS0")
			t.Logf("4 MiB to the serial console: %v", time.Since(start))
			time.Sleep(1500 * time.Millisecond) // a rotation after the last write
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
	if err := d.Stop(ctx, id); !errors.Is(err, backend.ErrUncleanStop) {
		t.Errorf("Stop without fsfreeze: %v, want ErrUncleanStop", err)
	} else {
		t.Logf("Stop without fsfreeze: %v", err)
	}
	if _, ok := running(l); ok {
		t.Error("firecracker still runs after an unclean Stop")
	}
}

// TestIntegrationPause pauses and resumes a 2 GiB sandbox with background
// processes running, then stops it while paused.
func TestIntegrationPause(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	d, id, l, run := newSandbox(t, ctx, backend.Spec{CPUs: 1, MemoryMB: 2048, DiskGB: 1})
	if err := d.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	state := func() backend.State {
		t.Helper()
		list, err := d.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, sb := range list {
			if sb.ID == id {
				return sb.State
			}
		}
		t.Fatalf("List does not show %s", id)
		return ""
	}
	pause := func() {
		t.Helper()
		start := time.Now()
		if err := d.Pause(ctx, id); err != nil {
			t.Fatal(err)
		}
		t.Logf("Pause: %v", time.Since(start))
		du, _ := exec.Command("du", "-h", l.mem, l.vmstate).CombinedOutput()
		t.Logf("snapshot on disk:\n%s", du)
		if got := state(); got != backend.StatePaused {
			t.Fatalf("after Pause: %s, want paused", got)
		}
	}
	sleeper := strings.TrimSpace(run("sleep 1000 >/dev/null 2>&1 & echo $!"))
	run("sh -c 'i=0; while :; do i=$((i+1)); echo $i > /tmp/count; sleep 0.1; done' >/dev/null 2>&1 &")
	// Not synced: Stop of a paused sandbox must still get it to the disk.
	run("echo kept > /root/unsynced")
	pause()
	if err := d.Pause(ctx, id); err != nil {
		t.Errorf("Pause of a paused sandbox: %v", err)
	}
	time.Sleep(2 * time.Second)
	// A new Driver, as after a daemon restart, resumes it.
	paths, err := artifacts.Fetch(ctx, filepath.Join(stateDir(t), artifacts.DirName))
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := New(ctx, stateDir(t), paths, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := restarted.Resume(ctx, id); err != nil {
		t.Fatal(err)
	}
	t.Logf("Resume of a 2 GiB sandbox until the agent answered: %v", time.Since(start))
	if got := state(); got != backend.StateRunning {
		t.Errorf("after Resume: %s, want running", got)
	}
	for _, p := range []string{l.vmstate, l.mem} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("Resume left %s: %v", p, err)
		}
	}
	run("kill -0 " + sleeper)
	run(`a=$(cat /tmp/count); sleep 0.5; b=$(cat /tmp/count); test "$b" -gt "$a"`)
	// The guest's clock must not lag by the time it spent paused.
	skew := strings.TrimSpace(run(fmt.Sprintf("echo $(( $(date +%%s) - %d ))", time.Now().Unix())))
	t.Logf("guest clock minus host clock after Resume: %s s", skew)
	if n, err := strconv.Atoi(skew); err != nil || n < -1 || n > 1 {
		t.Errorf("guest clock off by %q s after Resume", skew)
	}
	if err := d.Resume(ctx, id); err != nil {
		t.Errorf("Resume of a running sandbox: %v", err)
	}

	pause()
	if err := d.Stop(ctx, id); err != nil {
		t.Fatal(err)
	}
	if log, _ := os.ReadFile(l.log); !bytes.Contains(log, []byte("exiting successfully")) {
		t.Errorf("Stop of a paused sandbox was not clean; firecracker's log:\n%s", log)
	}
	if got := state(); got != backend.StateStopped {
		t.Errorf("after Stop: %s, want stopped", got)
	}
	if err := d.Resume(ctx, id); err == nil || !strings.Contains(err.Error(), "not paused") {
		t.Errorf("Resume of a stopped sandbox: %v, want not paused", err)
	}
	if err := d.Pause(ctx, id); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Errorf("Pause of a stopped sandbox: %v, want not running", err)
	}
	if err := d.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	if got := run("cat /root/unsynced"); got != "kept\n" {
		t.Errorf("file written before Pause and Stop: %q, want kept", got)
	}
	if err := d.Stop(ctx, id); err != nil {
		t.Error(err)
	}
}

// newSandbox creates a sandbox named it-<id>, removed when the test ends,
// and returns run, which runs a shell script in it and fails the test if
// the script fails.
func newSandbox(t *testing.T, ctx context.Context, spec backend.Spec) (*Driver, string, layout, func(string) string) {
	t.Helper()
	if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err != nil {
		t.Skipf("needs read/write /dev/kvm: %v", err)
	} else {
		f.Close()
	}
	home := stateDir(t)
	paths, err := artifacts.Fetch(ctx, filepath.Join(home, artifacts.DirName))
	if err != nil {
		t.Fatal(err)
	}
	d, err := New(ctx, home, paths, nil)
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 6)
	rand.Read(b)
	id := hex.EncodeToString(b)
	spec.Name = "it-" + id
	if _, err := d.Create(ctx, id, spec); err != nil {
		t.Fatal(err)
	}
	l := sandboxLayout(d.root, id)
	t.Cleanup(func() {
		if t.Failed() {
			for _, p := range []string{l.console, l.log} {
				b, _ := os.ReadFile(p)
				t.Logf("%s:\n%s", p, b)
			}
		}
		if err := d.Remove(context.Background(), id); err != nil {
			t.Error(err)
		}
		if _, err := os.Stat(l.dir); !os.IsNotExist(err) {
			t.Errorf("Remove left %s: %v", l.dir, err)
		}
	})
	c := agent.NewClient(func(ctx context.Context) (net.Conn, error) { return d.Dial(ctx, id) })
	t.Cleanup(c.CloseIdleConnections)
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
	return d, id, l, run
}

// fsck checks that Stop left the sandbox's disk consistent.
func fsck(t *testing.T, rootfs string) {
	t.Helper()
	e2fsck, err := exec.LookPath("e2fsck")
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
