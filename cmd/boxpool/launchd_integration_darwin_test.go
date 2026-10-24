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

// TestLaunchAgent runs upAgent and stopAgent, the launchd half of up and
// down, against the real launchd with a test label and a temporary
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
		lock:  filepath.Join(tmp, agentLabel+".test.lock"),
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
		if err := upAgent(a, exe, bin); err != nil {
			t.Fatal(err)
		}
		s, ok, err := printService(a)
		if err != nil || !ok || s.state != "running" || s.pid == 0 || s.program != a.bin {
			t.Fatalf("launchctl print %s = %+v, %v, %v; want %s running", a.target(), s, ok, err, a.bin)
		}
		return s.pid
	}

	helpers := vmHelpers(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	// sandbox creates the sandbox, or starts it again, so a VM is running.
	sandbox := func() {
		t.Helper()
		if _, err := c.CreateSandbox(ctx, daemon.CreateRequest{Name: "launchd", CPUs: 1, MemoryMB: 512, DiskGB: 1}); err != nil {
			t.Fatal(err)
		}
		if n := vmHelpers(t); n <= helpers {
			t.Fatalf("no VM runs after starting the sandbox: %d VM helpers, %d before the test", n, helpers)
		}
	}
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

	// A signed binary modified afterwards still has its entitlements, but
	// must not replace the installed one.
	good, err := os.ReadFile(a.bin)
	if err != nil {
		t.Fatal(err)
	}
	bad := append([]byte(nil), bin...)
	bad[len(bad)/2] ^= 0xff // in the code, well before the signature
	badExe := filepath.Join(tmp, "boxpool-modified")
	if err := os.WriteFile(badExe, bad, 0755); err != nil {
		t.Fatal(err)
	}
	if err := upAgent(a, badExe, bad); err == nil || !strings.Contains(err.Error(), "invalid signature") {
		t.Errorf("upAgent(modified) = %v; want an invalid signature refusal", err)
	}
	if now, _ := os.ReadFile(a.bin); string(now) != string(good) {
		t.Error("a binary that failed codesign -v replaced the installed one")
	}
	if s, _, _ := printService(a); s.pid != pid {
		t.Errorf("upAgent(modified) restarted the daemon: pid %d -> %d", pid, s.pid)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(a.bin), ".*")); len(left) != 0 {
		t.Errorf("staged copies left: %v", left)
	}
	t.Log("modified signed binary: refused")

	// A second up or down while one runs is refused, and changes nothing.
	unlock, err := lockAgent(a)
	if err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{"upAgent": upAgent(a, exe, bin), "stopAgent": stopAgent(a)} {
		if err == nil || !strings.Contains(err.Error(), "another boxpool up or down is running") {
			t.Errorf("%s while locked = %v; want it refused", name, err)
		}
	}
	unlock()
	if s, ok, _ := printService(a); !ok || s.pid != pid {
		t.Errorf("a refused up or down changed the agent: %+v, loaded %v; want pid %d", s, ok, pid)
	}
	if _, err := os.Stat(a.plist); err != nil {
		t.Errorf("a refused down removed the plist: %v", err)
	}
	// Run both at once: whatever wins, launchd and the plist agree.
	errs := make(chan error, 2)
	go func() { errs <- upAgent(a, exe, bin) }()
	go func() { errs <- stopAgent(a) }()
	for range 2 {
		if err := <-errs; err != nil && !strings.Contains(err.Error(), "another boxpool up or down is running") {
			t.Errorf("concurrent up and down: %v", err)
		}
	}
	_, loaded, _ := printService(a)
	_, statErr := os.Stat(a.plist)
	if loaded != (statErr == nil) {
		t.Errorf("after a concurrent up and down: loaded %v, plist %v", loaded, statErr)
	}
	t.Logf("concurrent up and down: loaded %v, plist %v", loaded, statErr == nil)
	pid = up(exe, bin)

	// Leaves a running VM for the binary upgrade below.
	sandbox()
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

	// With a VM running, the old daemon outlives launchctl bootout.
	sandbox()
	a.log = filepath.Join(tmp, "logs2", "daemon.log") // changes the plist
	if next = up(exe, bin); next == pid {
		t.Errorf("a plist change did not restart the daemon: pid %d", pid)
	}
	if syscall.Kill(pid, 0) == nil {
		t.Errorf("after a plist change, the old daemon pid %d is still running", pid)
	}
	t.Logf("plist changed with a VM running: pid %d -> %d", pid, next)
	pid = next

	// launchd still runs another path, from an earlier plist, while the
	// files on disk are current: up reloads rather than kickstarts it.
	other := a
	other.bin = filepath.Join(tmp, "other", "boxpool")
	other.plist = filepath.Join(tmp, "other", "agent.plist")
	if err := os.MkdirAll(filepath.Dir(other.bin), 0750); err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string][]byte{other.bin: bin, other.plist: agentPlist(other)} {
		if err := os.WriteFile(path, data, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := bootout(a, pid); err != nil {
		t.Fatal(err)
	}
	if err := launchctl("bootstrap", a.domain(), other.plist); err != nil {
		t.Fatal(err)
	}
	if err := waitHealthy(c, 2*time.Minute, func() error { return agentFailed(a) }); err != nil {
		t.Fatal(err)
	}
	next = up(exe, bin) // checks the program
	t.Logf("other program path loaded: reloaded, pid %d -> %d", pid, next)
	pid = next

	// A daemon run by hand on the same state directory answers health
	// checks while the agent cannot start: up refuses.
	if err := stopAgent(a); err != nil {
		t.Fatal(err)
	}
	fg := exec.Command(a.bin, "daemon")
	fg.Env = append(os.Environ(), "BOXPOOL_HOME="+home)
	if err := fg.Start(); err != nil {
		t.Fatal(err)
	}
	stopFG := func() {
		fg.Process.Signal(syscall.SIGTERM)
		fg.Wait()
	}
	defer stopFG()
	if err := waitHealthy(c, 2*time.Minute, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := upAgent(a, exe, bin); err == nil || !strings.Contains(err.Error(), "not the launch agent") {
		t.Errorf("upAgent with a foreground daemon = %v; want a refusal naming it", err)
	} else {
		t.Logf("foreground daemon: %v", err)
	}
	stopFG()
	pid = up(exe, bin)
	t.Logf("after the foreground daemon stopped: pid %d", pid)

	// With a VM running, down returns only after the daemon has exited.
	sandbox()
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
