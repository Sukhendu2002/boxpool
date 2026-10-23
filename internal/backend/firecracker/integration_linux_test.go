//go:build linux && integration

package firecracker

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
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
	if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err != nil {
		t.Skipf("needs read/write /dev/kvm: %v", err)
	} else {
		f.Close()
	}
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
	if _, err := d.Create(ctx, id, backend.Spec{Name: "it-" + id, CPUs: 2, MemoryMB: 512}); err != nil {
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
		// Firecracker logs this only when the guest ended the VM, not on a signal.
		if log, _ := os.ReadFile(l.log); !bytes.Contains(log, []byte("exiting successfully")) {
			t.Errorf("firecracker did not exit cleanly; its log:\n%s", log)
		}
		if _, ok := running(l); ok {
			t.Fatal("firecracker still runs after Stop")
		}
	}

	for boot := 1; boot <= 2; boot++ {
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
			out := run("uname -a")
			t.Logf("uname -a: %s", strings.TrimSpace(out))
			if !strings.HasPrefix(out, "Linux ") {
				t.Errorf("uname -a: %q", out)
			}
			// No sync: Stop must flush it, or the next boot will not see it.
			run("echo kept > /root/stop-test")
		} else if got := run("cat /root/stop-test"); got != "kept\n" {
			t.Errorf("file written before Stop: %q, want kept", got)
		}
		stop()
	}
}
