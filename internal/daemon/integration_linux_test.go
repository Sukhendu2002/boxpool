//go:build linux && integration

package daemon_test

import (
	"bytes"
	"context"
	"os"
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
	if err := stop(); err != nil {
		t.Fatal(err)
	}
}

// removeAll removes every sandbox the daemon in dir left, through a new
// driver; Firecracker's kills their VMs first.
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
