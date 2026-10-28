//go:build linux && integration

package daemon_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"boxpool.run/boxpool/internal/agent"
	"boxpool.run/boxpool/internal/artifacts"
	"boxpool.run/boxpool/internal/backend/docker"
	"boxpool.run/boxpool/internal/client"
	"boxpool.run/boxpool/internal/daemon"
)

// TestIntegrationSh runs commands through a real daemon and backend, as
// boxpool sh does, and removes the sandbox afterwards.
func TestIntegrationSh(t *testing.T) {
	t.Run("firecracker", func(t *testing.T) {
		if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err != nil {
			t.Skipf("needs read/write /dev/kvm: %v", err)
		} else {
			f.Close()
		}
		dir := stateDir(t)
		// Share the Firecracker driver test's artifact cache.
		cache := os.Getenv("BOXPOOL_TEST_HOME")
		if cache == "" {
			c, err := os.UserCacheDir()
			if err != nil {
				t.Fatal(err)
			}
			cache = filepath.Join(c, "boxpool-test")
		}
		cache = filepath.Join(cache, artifacts.DirName)
		if err := os.MkdirAll(cache, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(dir, 0750); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(cache, filepath.Join(dir, artifacts.DirName)); err != nil {
			t.Fatal(err)
		}
		integrationSh(t, dir, "firecracker")
	})
	t.Run("docker", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := docker.Probe(ctx); err != nil {
			if os.Getenv("BOXPOOL_TEST_REQUIRE_DOCKER") == "1" {
				t.Fatal(err)
			}
			t.Skipf("needs a Docker Engine: %v", err)
		}
		integrationSh(t, stateDir(t), "docker")
	})
}

func integrationSh(t *testing.T, dir, backend string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	stop := start(t, dir, daemon.Info{Backend: backend})
	c := client.New(dir)
	sb, err := c.CreateSandbox(ctx, daemon.CreateRequest{CPUs: 1, MemoryMB: 512, DiskGB: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeAll(t, dir, backend) })
	t.Logf("sandbox %s (%s)", sb.Name, sb.ID)
	if sb.Backend != backend {
		t.Errorf("backend %q, want %q", sb.Backend, backend)
	}
	if sb.CPUs != 1 || sb.MemoryMB != 512 || sb.DiskGB != 1 {
		t.Fatalf("requested limits were not stored: %+v", sb)
	}
	run := func(script string) string {
		t.Helper()
		var out bytes.Buffer
		s, err := c.Exec(ctx, sb.Name, agent.ExecRequest{Argv: []string{"sh", "-c", script}}, &out, &out)
		if err != nil {
			t.Fatal(err)
		}
		if code, err := s.Wait(); code != 0 || err != nil {
			t.Fatalf("%s: exit %d, %v: %s", script, code, err, out.String())
		}
		return out.String()
	}
	if got := run("echo ok"); got != "ok\n" {
		t.Errorf("echo ok printed %q", got)
	}
	t.Logf("nproc: %s", strings.TrimSpace(run(`test "$(nproc)" -eq 1 && nproc`)))
	if backend == "firecracker" {
		t.Logf("guest resources:\n%s", run(`set -eu
awk '/^MemTotal:/ {print; found=1; if ($2 < 419430 || $2 > 524288) exit 1} END {if (!found) exit 1}' /proc/meminfo
test "$(blockdev --getsize64 /dev/vda)" -eq 1073741824
df -k /`))
	} else {
		t.Logf("memory cgroup limit:\n%s", run(`set -eu
if test -r /sys/fs/cgroup/memory.max; then limit=$(cat /sys/fs/cgroup/memory.max)
else limit=$(cat /sys/fs/cgroup/memory/memory.limit_in_bytes); fi
test "$limit" -eq 536870912
printf '%s\n' "$limit"`))
	}
	for _, oversized := range []struct {
		name     string
		req      daemon.CreateRequest
		resource string
	}{
		{"too-many-cpus", daemon.CreateRequest{CPUs: 1 << 20, MemoryMB: 512, DiskGB: 1}, "CPU"},
		{"too-much-memory", daemon.CreateRequest{CPUs: 1, MemoryMB: 1 << 30, DiskGB: 1}, "memory"},
	} {
		req := oversized.req
		req.Name = oversized.name
		if _, err := c.CreateSandbox(ctx, req); err == nil || !strings.Contains(err.Error(), "insufficient "+oversized.resource) {
			t.Fatalf("excess %s request: %v", oversized.resource, err)
		}
		if _, err := c.GetSandbox(ctx, req.Name); err == nil {
			t.Errorf("refused %s request left a sandbox record", oversized.resource)
		}
	}
	run("echo kept > /root/marker")

	// Attaching again by name reaches the same, still running sandbox.
	again, err := c.CreateSandbox(ctx, daemon.CreateRequest{Name: sb.Name})
	if err != nil || again.ID != sb.ID {
		t.Fatalf("second create = %+v, %v; want ID %s", again, err, sb.ID)
	}
	if got := run("cat /root/marker"); got != "kept\n" {
		t.Errorf("marker = %q", got)
	}

	// boxpool exec passes the exit code through.
	s, err := c.Exec(ctx, sb.Name, agent.ExecRequest{Argv: []string{"sh", "-c", "exit 7"}}, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if code, err := s.Wait(); code != 7 || err != nil {
		t.Errorf("exit 7 = %d, %v", code, err)
	}
	copyRoundTrip(t, ctx, c, sb.Name)

	// ls, stop and rm, across a daemon restart.
	other, err := c.CreateSandbox(ctx, daemon.CreateRequest{Name: "other", CPUs: 1, MemoryMB: 512, DiskGB: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(t, c); got != sb.Name+"=running other=running" {
		t.Errorf("ls = %q", got)
	}
	if _, err := c.RemoveSandbox(ctx, sb.Name, false); err == nil || !strings.Contains(err.Error(), "is running") {
		t.Errorf("rm of a running sandbox = %v", err)
	}
	if stopped, err := c.StopSandbox(ctx, sb.Name); err != nil || stopped.State != "stopped" {
		t.Fatalf("stop = %+v, %v", stopped, err)
	} else if stopped.Warning != "" {
		t.Logf("stop warning: %s", stopped.Warning)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	stop = start(t, dir, daemon.Info{Backend: backend})
	// The new daemon adopts the sandbox that kept running.
	if got := names(t, c); got != sb.Name+"=stopped other=running" {
		t.Errorf("ls after a daemon restart = %q", got)
	}
	var out bytes.Buffer
	if s, err := c.Exec(ctx, "other", agent.ExecRequest{Argv: []string{"echo", "adopted"}}, &out, &out); err != nil {
		t.Errorf("exec in the adopted sandbox: %v", err)
	} else if code, err := s.Wait(); code != 0 || err != nil || out.String() != "adopted\n" {
		t.Errorf("exec in the adopted sandbox: exit %d, %v: %q", code, err, out.String())
	}
	// start boots the stopped sandbox with the disk it had before stop.
	if started, err := c.StartSandbox(ctx, sb.Name); err != nil || started.State != "running" {
		t.Fatalf("start = %+v, %v", started, err)
	}
	if got := run("cat /root/marker"); got != "kept\n" {
		t.Errorf("marker after stop and start = %q", got)
	}
	// pause keeps the guest's memory, so it does not reboot; Docker cannot,
	// so pause stops the container and says so.
	want := "paused"
	if backend == "docker" {
		want = "stopped"
	}
	bootID := run("cat /proc/sys/kernel/random/boot_id")
	if paused, err := c.PauseSandbox(ctx, sb.Name); err != nil {
		t.Fatal(err)
	} else if paused.State != want || (paused.Warning != "") != (backend == "docker") {
		t.Errorf("pause = %+v", paused)
	}
	if got := names(t, c); got != sb.Name+"="+want+" other=running" {
		t.Errorf("ls after pause = %q", got)
	}
	if resumed, err := c.ResumeSandbox(ctx, sb.Name); err != nil || resumed.State != "running" {
		t.Fatalf("resume = %+v, %v", resumed, err)
	}
	if got := run("cat /proc/sys/kernel/random/boot_id"); backend != "docker" && got != bootID {
		t.Errorf("boot id after pause and resume = %q, want %q", got, bootID)
	}
	if got := run("cat /root/marker"); got != "kept\n" {
		t.Errorf("marker after pause and resume = %q", got)
	}
	if _, err := c.StopSandbox(ctx, sb.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateSandbox(ctx, daemon.CreateRequest{Name: "other"}); err != nil {
		t.Fatal(err)
	}
	for _, rm := range []struct {
		name  string
		force bool
	}{{sb.Name, false}, {"other", true}} {
		if _, err := c.RemoveSandbox(ctx, rm.name, rm.force); err != nil {
			t.Errorf("rm %s = %v", rm.name, err)
		}
	}
	if got := names(t, c); got != "" {
		t.Errorf("ls after rm = %q", got)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	// The drivers freed everything: no VM, container, disk or reservation.
	if backend == "firecracker" {
		for _, id := range []string{sb.ID, other.ID} {
			if _, err := os.Stat(filepath.Join(dir, "sandboxes", id)); !os.IsNotExist(err) {
				t.Errorf("sandbox %s directory: %v", id, err)
			}
			if out, _ := exec.Command("pgrep", "-f", id).Output(); len(out) != 0 {
				t.Errorf("processes left for sandbox %s: %s", id, out)
			}
		}
	}
	d, err := daemon.OpenDriver(ctx, dir, &daemon.Info{Backend: backend}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if list, err := d.List(ctx); err != nil || len(list) != 0 {
		t.Errorf("driver still lists %+v, %v", list, err)
	}
}

// names returns the daemon's sandboxes as name=state pairs.
func names(t *testing.T, c *client.Client) string {
	t.Helper()
	list, err := c.ListSandboxes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var s []string
	for _, sb := range list {
		s = append(s, sb.Name+"="+string(sb.State))
	}
	return strings.Join(s, " ")
}

// copyRoundTrip uploads a directory to /tmp in the sandbox, downloads it
// again and compares the two, as boxpool cp does.
func copyRoundTrip(t *testing.T, ctx context.Context, c *client.Client, name string) {
	t.Helper()
	host := t.TempDir()
	src := filepath.Join(host, "dir")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("hello\n"), 0640)
	os.WriteFile(filepath.Join(src, "sub", "big"), bytes.Repeat([]byte("0123456789abcdef"), 1<<16), 0755)
	os.Symlink("../a.txt", filepath.Join(src, "sub", "link"))
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(agent.WriteTar(pw, src)) }()
	if err := c.Upload(ctx, name, "/tmp", pr); err != nil {
		t.Fatal(err)
	}
	tr, err := c.Download(ctx, name, "/tmp/dir")
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	out := filepath.Join(host, "out")
	os.Mkdir(out, 0755)
	if err := agent.ExtractTar(tr, out, nil); err != nil {
		t.Fatal(err)
	}
	if b, err := exec.Command("diff", "-r", "--no-dereference", src, filepath.Join(out, "dir")).CombinedOutput(); err != nil {
		t.Errorf("diff -r: %v\n%s", err, b)
	}
	for _, f := range []string{"a.txt", "sub/big"} {
		a, _ := os.Lstat(filepath.Join(src, f))
		b, err := os.Lstat(filepath.Join(out, "dir", f))
		if err != nil || a.Mode() != b.Mode() || !a.ModTime().Equal(b.ModTime()) {
			t.Errorf("%s: %v %v, want %v %v (%v)", f, b.Mode(), b.ModTime(), a.Mode(), a.ModTime(), err)
		}
	}
}

// removeAll removes every sandbox the daemon in dir left, through a new
// driver, whose Remove kills a running VM first.
func removeAll(t *testing.T, dir, backend string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	d, err := daemon.OpenDriver(ctx, dir, &daemon.Info{Backend: backend}, nil)
	if err != nil {
		t.Error(err)
		return
	}
	list, err := d.List(ctx)
	if err != nil {
		t.Error(err)
	}
	for _, sb := range list {
		if err := d.Remove(ctx, sb.ID); err != nil {
			t.Error(err)
		}
	}
}
