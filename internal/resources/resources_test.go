//go:build linux || darwin

package resources

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"boxpool.run/boxpool/internal/backend"
)

func capacity() Capacity {
	return Capacity{CPUs: []int{2, 4, 6, 8}, MemoryMB: 8192, AvailableMB: 8192, DiskBytes: 40 << 30}
}

func TestVZReservations(t *testing.T) {
	m := &Manager{StateDir: t.TempDir(), Probe: func(string) (Capacity, error) { return capacity(), nil }}
	spec := backend.Spec{CPUs: 1, MemoryMB: 512, DiskGB: 1}
	ctx := context.Background()
	if _, err := m.Create(ctx, "vz", "first", spec, 1<<30); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(m.StateDir, "sandboxes", "first", manifest)); err != nil {
		t.Fatal(err)
	}
	// Reading the shared VM directory twice would charge each VM twice.
	allocations, err := m.allocations()
	if err != nil || len(allocations) != 1 || allocations[0].DiskBytes != 1<<30 {
		t.Fatalf("vz reservation: %+v, %v", allocations, err)
	}
	if _, err := m.Create(ctx, "vz", "second", backend.Spec{CPUs: 3, MemoryMB: 512}, 1<<30); err == nil {
		t.Error("ignored existing vz reservation")
	}
}

func TestAdmit(t *testing.T) {
	spec := backend.Spec{CPUs: 1, MemoryMB: 512, DiskGB: 1}
	for _, tc := range []struct {
		name     string
		change   func(*Capacity)
		existing []Allocation
		request  backend.Spec
		disk     int64
		want     string
	}{
		{name: "free", request: spec, disk: 1 << 30},
		{name: "defaults", request: backend.Spec{}, disk: 10 << 30},
		{name: "CPU reserve", change: func(c *Capacity) { c.CPUs = []int{7} }, request: spec, want: "insufficient CPU"},
		{name: "existing stopped CPU", existing: []Allocation{{Spec: backend.Spec{CPUs: 3}}}, request: spec, want: "insufficient CPU"},
		{name: "memory total reserve", change: func(c *Capacity) { c.MemoryMB = 1535; c.AvailableMB = 1535 }, request: spec, want: "insufficient memory"},
		{name: "memory reserve exact", change: func(c *Capacity) { c.MemoryMB = 1536; c.AvailableMB = 1536 }, request: spec},
		{name: "memory live pressure", change: func(c *Capacity) { c.AvailableMB = 1535 }, request: spec, want: "insufficient memory"},
		{name: "ten percent reserve", change: func(c *Capacity) { c.MemoryMB = 20481; c.AvailableMB = 2560 }, request: spec, want: "2049 MiB host reserve"},
		{name: "existing memory", existing: []Allocation{{Spec: backend.Spec{CPUs: 1, MemoryMB: 7000}}}, request: spec, want: "insufficient memory"},
		{name: "pending commitment under host pressure", change: func(c *Capacity) { c.AvailableMB = 3072 }, existing: []Allocation{{Spec: backend.Spec{CPUs: 1, MemoryMB: 2048}}}, request: spec, want: "insufficient memory"},
		{name: "existing plus reserve exact", change: func(c *Capacity) { c.AvailableMB = 3584 }, existing: []Allocation{{Spec: backend.Spec{CPUs: 1, MemoryMB: 2048}}}, request: spec},
		{name: "disk", request: spec, disk: 41 << 30, want: "insufficient disk"},
		{name: "existing disk", existing: []Allocation{{Spec: backend.Spec{CPUs: 1, MemoryMB: 512}, DiskBytes: 40 << 30}}, request: spec, disk: 1 << 30, want: "insufficient disk"},
		{name: "docker disk limitation", change: func(c *Capacity) { c.DiskBytes = 0 }, request: spec},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := capacity()
			if tc.change != nil {
				tc.change(&c)
			}
			a, err := Admit(c, tc.existing, tc.request, tc.disk)
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("got %v, want %s", err, tc.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(a.CPUs) != a.Spec.CPUs || a.CPUs[0] == c.CPUs[0] {
				t.Errorf("CPU allocation %+v", a)
			}
		})
	}
}

func TestManagerReservations(t *testing.T) {
	ctx := context.Background()
	m := &Manager{StateDir: t.TempDir(), Probe: func(string) (Capacity, error) { return capacity(), nil }}
	// Independent Manager instances and both backends use the same lock.
	var wg sync.WaitGroup
	var mu sync.Mutex
	var admitted []string
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m2 := &Manager{StateDir: m.StateDir, Probe: m.Probe}
			kind := "docker"
			disk := int64(0)
			if i%2 == 0 {
				kind = "firecracker"
				disk = 1 << 30
			}
			id := fmt.Sprint(i)
			_, err := m2.Create(ctx, kind, id, backend.Spec{CPUs: 1, MemoryMB: 512}, disk)
			if err != nil {
				if !strings.Contains(err.Error(), "insufficient CPU") {
					t.Error(err)
				}
				return
			}
			mu.Lock()
			admitted = append(admitted, filepath.Join(m.StateDir, directory(kind), id))
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	if len(admitted) != 3 {
		t.Fatalf("admitted %d sandboxes, want 3", len(admitted))
	}
	allocations, err := m.allocations()
	if err != nil {
		t.Fatal(err)
	}
	var cpuIDs []int
	for _, a := range allocations {
		cpuIDs = append(cpuIDs, a.CPUs...)
	}
	if len(cpuIDs) != 3 || cpuIDs[0] == cpuIDs[1] || cpuIDs[0] == cpuIDs[2] || cpuIDs[1] == cpuIDs[2] {
		t.Errorf("overlapping CPU sets %v", cpuIDs)
	}
	// Reopening preserves stopped/pending reservations; only Remove releases.
	reopened := &Manager{StateDir: m.StateDir, Probe: m.Probe}
	if _, err := reopened.Create(ctx, "docker", "extra", backend.Spec{CPUs: 1, MemoryMB: 512}, 0); err == nil {
		t.Fatal("restart lost allocations")
	}
	if err := os.RemoveAll(admitted[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Create(ctx, "docker", "extra", backend.Spec{CPUs: 1, MemoryMB: 512}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Create(ctx, "docker", "extra", backend.Spec{CPUs: 1, MemoryMB: 512}, 0); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("duplicate: %v", err)
	}
}

func TestManagerLegacyAndCorruption(t *testing.T) {
	ctx := context.Background()
	m := &Manager{StateDir: t.TempDir(), Probe: func(string) (Capacity, error) { return capacity(), nil }}
	dir := filepath.Join(m.StateDir, "sandboxes", "legacy")
	os.MkdirAll(dir, 0700)
	os.WriteFile(filepath.Join(dir, "spec.json"), []byte(`{"CPUs":3,"MemoryMB":512}`), 0600)
	os.WriteFile(filepath.Join(dir, "rootfs.ext4"), []byte("old image"), 0600)
	if _, err := m.Create(ctx, "docker", "new", backend.Spec{CPUs: 1, MemoryMB: 512}, 0); err == nil || !strings.Contains(err.Error(), "insufficient CPU") {
		t.Fatalf("legacy VM ignored: %v", err)
	}
	os.RemoveAll(dir)
	unknown := filepath.Join(m.StateDir, "docker", "unknown")
	os.MkdirAll(unknown, 0700)
	if _, err := m.Create(ctx, "firecracker", "new", backend.Spec{CPUs: 1, MemoryMB: 512}, 1<<30); err == nil || !strings.Contains(err.Error(), "lacks resources.json") {
		t.Fatalf("unaccounted Docker sandbox ignored: %v", err)
	}
	os.RemoveAll(unknown)
	a := Allocation{Spec: backend.Spec{CPUs: 3, MemoryMB: 512}}
	if err := m.Adopt(ctx, "docker", "old", a); err != nil {
		t.Fatal(err)
	}
	if err := m.Adopt(ctx, "docker", "old", Allocation{}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(ctx, "docker", "new", backend.Spec{CPUs: 1, MemoryMB: 512}, 0); err == nil {
		t.Fatal("adopted allocation overwritten or ignored")
	}
	os.WriteFile(filepath.Join(m.StateDir, "docker", "old", manifest), []byte("broken"), 0600)
	if _, err := m.Create(ctx, "docker", "new", backend.Spec{CPUs: 1}, 0); err == nil || !strings.Contains(err.Error(), "read allocation") {
		t.Fatalf("corruption should fail closed: %v", err)
	}
}

func TestManagerCanceledLockAndProbeError(t *testing.T) {
	m := &Manager{StateDir: t.TempDir(), Probe: func(string) (Capacity, error) { return capacity(), nil }}
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- m.locked(context.Background(), func() error { close(entered); <-release; return nil }) }()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := m.Create(ctx, "docker", "x", backend.Spec{}, 0)
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("locked create: %v", err)
	}
	m.Probe = func(string) (Capacity, error) { return Capacity{}, errors.New("probe failed") }
	if _, err := m.Create(context.Background(), "docker", "x", backend.Spec{}, 0); err == nil || !strings.Contains(err.Error(), "probe failed") {
		t.Fatalf("probe error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(m.StateDir, "docker", "x")); !os.IsNotExist(err) {
		t.Errorf("failed admission created directory: %v", err)
	}
}

func TestCPUSet(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []int
	}{{"", nil}, {"2,4-6,4", []int{2, 4, 5, 6}}, {"0", []int{0}}} {
		got, err := ParseCPUSet(tc.in)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parse %q: %v, %v", tc.in, got, err)
		}
	}
	for _, in := range []string{"-1", "2-1", "x", "1,", "0-10000000"} {
		if _, err := ParseCPUSet(in); err == nil {
			t.Errorf("accepted %q", in)
		}
	}
}

func TestFree(t *testing.T) {
	m := &Manager{StateDir: t.TempDir(), Probe: func(string) (Capacity, error) { return capacity(), nil }}
	ctx := context.Background()
	// 4 CPUs less 1 reserved, 8 GiB less the 1 GiB reserve.
	if cpus, mem, err := m.Free(ctx); err != nil || cpus != 3 || mem != 7168 {
		t.Errorf("Free() on an empty node = %d, %d, %v; want 3, 7168", cpus, mem, err)
	}
	if _, err := m.Create(ctx, "vz", "a", backend.Spec{CPUs: 2, MemoryMB: 2048, DiskGB: 1}, 1<<30); err != nil {
		t.Fatal(err)
	}
	if cpus, mem, err := m.Free(ctx); err != nil || cpus != 1 || mem != 5120 {
		t.Errorf("Free() after a sandbox = %d, %d, %v; want 1, 5120", cpus, mem, err)
	}
}
