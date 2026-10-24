//go:build integration && darwin

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"boxpool.run/boxpool/internal/client"
	"boxpool.run/boxpool/internal/daemon"
)

// TestLaunchAgent runs installAgent and stopAgent, the launchd half of up
// and down, against the real launchd with a test label and a temporary
// state directory, so it skips preflight and leaves the user's agent alone:
//
//	go test -tags integration -run TestLaunchAgent -v ./cmd/boxpool
func TestLaunchAgent(t *testing.T) {
	tmp, err := os.MkdirTemp("", "bp") // short: macOS caps socket paths at ~104 bytes
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(tmp, "h")
	a := launchAgent{
		label: agentLabel + ".test",
		plist: filepath.Join(tmp, agentLabel+".test.plist"),
		bin:   filepath.Join(home, "bin", "boxpool"),
		log:   filepath.Join(tmp, "logs", "daemon.log"),
		home:  home,
	}
	t.Cleanup(func() {
		if err := stopAgent(a); err != nil {
			t.Error(err)
		}
		if t.Failed() {
			if b, err := os.ReadFile(a.log); err == nil {
				t.Logf("daemon log:\n%s", b)
			}
		}
		os.RemoveAll(tmp)
	})
	if err := os.MkdirAll(home, 0750); err != nil {
		t.Fatal(err)
	}
	// Reuse the vz integration test's artifact cache when there is one.
	if cache := filepath.Join(os.Getenv("HOME"), "Library", "Caches", "boxpool-test", "artifacts"); dirExists(cache) {
		if err := os.Symlink(cache, filepath.Join(home, "artifacts")); err != nil {
			t.Fatal(err)
		}
	}

	build := func(version string, sign bool) (string, []byte) {
		exe := filepath.Join(tmp, "boxpool-"+version)
		if out, err := exec.Command("go", "build", "-ldflags", "-X main.version="+version, "-o", exe, ".").CombinedOutput(); err != nil {
			t.Fatalf("go build: %v\n%s", err, out)
		}
		if sign {
			if out, err := exec.Command("../../scripts/dev-sign.sh", exe).CombinedOutput(); err != nil {
				t.Fatalf("dev-sign.sh: %v\n%s", err, out)
			}
		}
		b, err := os.ReadFile(exe)
		if err != nil {
			t.Fatal(err)
		}
		return exe, b
	}
	c := client.New(home)
	up := func(exe string, bin []byte) int {
		t.Helper()
		if err := installAgent(a, exe, bin); err != nil {
			t.Fatal(err)
		}
		if err := waitHealthy(c, 2*time.Minute, func() error { return agentFailed(a) }); err != nil {
			t.Fatal(err)
		}
		s, ok, err := printService(a)
		if err != nil || !ok || s.state != "running" || s.pid == 0 {
			t.Fatalf("launchctl print %s = %+v, %v, %v; want running", a.target(), s, ok, err)
		}
		return s.pid
	}

	helpers := vmHelpers(t)
	exe, bin := build("unsigned", false)
	if err := installAgent(a, exe, bin); err == nil || !strings.Contains(err.Error(), "scripts/dev-sign.sh") {
		t.Fatalf("installAgent(unsigned) = %v; want a refusal that points to scripts/dev-sign.sh", err)
	}

	exe, bin = build("v1", true)
	pid := up(exe, bin)
	t.Logf("first up: pid %d", pid)

	if again := up(exe, bin); again != pid {
		t.Errorf("a no-op up restarted the daemon: pid %d, then %d", pid, again)
	} else {
		t.Logf("no-op up: pid %d kept", again)
	}

	// Leaves a running VM for the stop below to clean up.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := c.CreateSandbox(ctx, daemon.CreateRequest{Name: "launchd", CPUs: 1, MemoryMB: 512, DiskGB: 1}); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(a.bin, "exec", "launchd", "--", "uname", "-m")
	cmd.Env = append(os.Environ(), "BOXPOOL_HOME="+home)
	if out, err := cmd.CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "aarch64" {
		t.Fatalf("boxpool exec launchd -- uname -m = %q, %v; want aarch64", out, err)
	}
	t.Logf("VM helpers: %d before, %d with the sandbox running", helpers, vmHelpers(t))

	exe, bin = build("v2", true)
	next := up(exe, bin)
	if info, err := c.Info(ctx); err != nil || info.Version != "v2" || next == pid {
		t.Errorf("after a binary upgrade: pid %d -> %d, version %q, %v; want a new pid running v2", pid, next, info.Version, err)
	}
	t.Logf("binary changed: pid %d -> %d", pid, next)
	pid = next

	a.log = filepath.Join(tmp, "logs2", "daemon.log") // changes the plist
	if next = up(exe, bin); next == pid {
		t.Errorf("a plist change did not restart the daemon: pid %d", pid)
	}
	t.Logf("plist changed: pid %d -> %d", pid, next)
	pid = next

	if err := stopAgent(a); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := printService(a); ok || err != nil {
		t.Errorf("after stopAgent, launchd still has %s (%v)", a.label, err)
	}
	if syscall.Kill(pid, 0) == nil {
		t.Errorf("after stopAgent, daemon pid %d is still running", pid)
	}
	if _, err := os.Stat(a.plist); !os.IsNotExist(err) {
		t.Errorf("after stopAgent, %s: %v; want it removed", a.plist, err)
	}
	if kept, _ := os.ReadDir(filepath.Join(home, "sandboxes")); len(kept) != 1 || !dirExists(filepath.Dir(a.bin)) {
		t.Error("stopAgent removed the state directory or the binary")
	}
	// The framework's helper ends with the daemon, maybe not at once.
	n := vmHelpers(t)
	for deadline := time.Now().Add(10 * time.Second); n > helpers && time.Now().Before(deadline); n = vmHelpers(t) {
		time.Sleep(200 * time.Millisecond)
	}
	if n > helpers {
		t.Errorf("after stopAgent, %d VM helpers run; %d did before the test", n, helpers)
	}
	t.Logf("down: pid %d gone, plist removed, state kept, %d VM helpers", pid, n)
}

// vmHelpers counts the Virtualization framework's per-VM helpers, which
// other programs on this Mac may also run.
func vmHelpers(t *testing.T) int {
	out, err := exec.Command("ps", "-axo", "comm").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(out), "com.apple.Virtualization.VirtualMachine")
}

func dirExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}
