//go:build linux || darwin

package agent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"
)

type proc struct {
	cmd  *exec.Cmd
	done chan struct{} // closed once err is set
	err  error
}

func TestShutdown(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "marker")
	// trap writes marker on SIGTERM and exits; stubborn ignores SIGTERM.
	trap := &proc{cmd: exec.Command("sh", "-c", `trap 'echo done > "$0"; exit 0' TERM; while :; do sleep 0.05; done`, marker)}
	stubborn := &proc{cmd: exec.Command("sh", "-c", `trap '' TERM; while :; do sleep 0.05; done`)}
	procs := []*proc{trap, stubborn}
	for _, p := range procs {
		if err := p.cmd.Start(); err != nil {
			t.Fatal(err)
		}
		p.done = make(chan struct{})
		go func() { p.err = p.cmd.Wait(); close(p.done) }()
		t.Cleanup(func() { p.cmd.Process.Kill() })
	}
	time.Sleep(100 * time.Millisecond) // until the traps are set
	var (
		mu                  sync.Mutex
		signals             = map[int][]syscall.Signal{}
		remounted, rebooted bool
	)
	c := serveShutdown(t, &Shutdown{
		Pids: func() ([]int, error) {
			pids := []int{1, os.Getpid()}
			for _, p := range procs {
				select {
				case <-p.done:
				default:
					pids = append(pids, p.cmd.Process.Pid)
				}
			}
			return pids, nil
		},
		Kill: func(pid int, sig syscall.Signal) error {
			mu.Lock()
			signals[pid] = append(signals[pid], sig)
			mu.Unlock()
			if pid == 1 || pid == os.Getpid() {
				t.Errorf("signalled pid %d with %v", pid, sig)
				return nil
			}
			return syscall.Kill(pid, sig)
		},
		Remount: func() error { remounted = true; return nil },
		Reboot:  func() error { rebooted = true; return nil },
	})

	v, err := c.Version(context.Background())
	if err != nil || !slices.Contains(v.Features, FeatureShutdown) {
		t.Fatalf("Version() = %+v, %v; want feature %q", v, err, FeatureShutdown)
	}
	grace := 300 * time.Millisecond
	start := time.Now()
	if err := c.Shutdown(context.Background(), grace); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took < grace || took > grace+3*time.Second {
		t.Errorf("Shutdown took %v with a %v grace period", took, grace)
	}
	if b, err := os.ReadFile(marker); string(b) != "done\n" {
		t.Errorf("marker after SIGTERM: %q, %v", b, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got := signals[trap.cmd.Process.Pid]; !slices.Equal(got, []syscall.Signal{syscall.SIGTERM}) {
		t.Errorf("trap process got %v, want SIGTERM only", got)
	}
	if got := signals[stubborn.cmd.Process.Pid]; !slices.Equal(got, []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL}) {
		t.Errorf("stubborn process got %v, want SIGTERM then SIGKILL", got)
	}
	var ee *exec.ExitError
	<-stubborn.done
	if err := stubborn.err; !errors.As(err, &ee) || ee.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Errorf("stubborn process: %v, want killed by SIGKILL", err)
	}
	if !remounted || !rebooted {
		t.Errorf("remounted %v, rebooted %v; want both", remounted, rebooted)
	}
}

func TestShutdownNotServed(t *testing.T) {
	c := serve(t)
	v, err := c.Version(context.Background())
	if err != nil || len(v.Features) != 0 {
		t.Errorf("Version() = %+v, %v; want no features", v, err)
	}
	if err := c.Shutdown(context.Background(), time.Second); err == nil {
		t.Error("Shutdown on an agent without it succeeded")
	}
}
