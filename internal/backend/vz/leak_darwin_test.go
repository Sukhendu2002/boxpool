//go:build darwin && cgo && integration

package vz

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strings"
	"testing"
	"time"

	"boxpool.run/boxpool/internal/backend"
)

// TestIntegrationNoGoroutineLeak checks that a released VM leaves no vz
// notification workers behind, after clean stops and both kinds of failed
// Start. Stock vz v3 kept about three per VM.
func TestIntegrationNoGoroutineLeak(t *testing.T) {
	d, _ := integrationSandbox(t)
	ctx := context.Background()
	cycle := func(id string) error {
		if _, err := d.Create(ctx, id, backend.Spec{CPUs: 1, MemoryMB: 512, DiskGB: 1}); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := d.Remove(ctx, id); err != nil {
				t.Fatal(err)
			}
		}()
		if err := d.Start(ctx, id); err != nil {
			return err
		}
		return d.Stop(ctx, id)
	}
	// The first VM starts process-wide framework and runtime goroutines.
	if err := cycle("warm"); err != nil {
		t.Fatal(err)
	}
	base := settledGoroutines(0)
	const cycles = 8
	for i := 0; i < cycles; i++ {
		if err := cycle("cycle"); err != nil {
			t.Fatal(err)
		}
	}
	// Readiness failure: the VM runs, the agent never answers in time.
	old := agentTimeout
	agentTimeout = time.Nanosecond
	err := cycle("unready")
	agentTimeout = old
	if err == nil {
		t.Fatal("Start succeeded with no time for the agent")
	}
	// Framework failure: VZVirtualMachine.start rejects a kernel that is not one.
	kernel := filepath.Join(t.TempDir(), "Image")
	if err := os.WriteFile(kernel, make([]byte, 1<<20), 0600); err != nil {
		t.Fatal(err)
	}
	d.kernel, kernel = kernel, d.kernel
	err = cycle("bad-kernel")
	d.kernel = kernel
	if err == nil || !strings.Contains(err.Error(), "start sandbox") {
		t.Fatalf("Start with a bad kernel: %v", err)
	}
	t.Logf("bad kernel: %v", err)
	const slack = 2
	got := settledGoroutines(base + slack)
	t.Logf("goroutines: %d before, %d after %d cycles, a readiness failure and a framework failure", base, got, cycles)
	if got > base+slack {
		var b strings.Builder
		pprof.Lookup("goroutine").WriteTo(&b, 1)
		t.Fatalf("%d goroutines leaked:\n%s", got-base, b.String())
	}
}

// settledGoroutines collects released VMs, so their finalizers run, until
// the count is at most want or 10 s pass.
func settledGoroutines(want int) int {
	n := 0
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		runtime.GC()
		time.Sleep(100 * time.Millisecond)
		if n = runtime.NumGoroutine(); n <= want {
			break
		}
	}
	return n
}
