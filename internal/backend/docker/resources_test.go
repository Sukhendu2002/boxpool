package docker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"boxpool.run/boxpool/internal/backend"
	"boxpool.run/boxpool/internal/resources"
)

func TestResourceDefaultsAndAdmission(t *testing.T) {
	ctx := context.Background()
	f := newFakeEngine(t)
	d := f.driver(ctx, f.serve())
	if _, err := d.Create(ctx, "default", backend.Spec{}); err != nil {
		t.Fatal(err)
	}
	h := f.containers[d.name("default")].cfg.HostConfig
	if h.NanoCPUs != 2e9 || h.Memory != 2048<<20 || h.MemorySwap != h.Memory || h.CpusetCpus != "1,2" {
		t.Fatalf("defaults not enforced: %+v", h)
	}
	var a resources.Allocation
	b, err := os.ReadFile(filepath.Join(d.root, "default", "resources.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &a); err != nil {
		t.Fatal(err)
	}
	if a.Spec.DiskGB != 10 || a.DiskBytes != 0 {
		t.Fatalf("disk default/limitation: %+v", a)
	}
	// Admission is before image pulls and Engine mutations.
	before := len(f.requests)
	if _, err := d.Create(ctx, "oversized", backend.Spec{CPUs: 8}); err == nil || !strings.Contains(err.Error(), "insufficient CPU") {
		t.Fatalf("CPU refusal: %v", err)
	}
	d.admission.Probe = func(string) (resources.Capacity, error) {
		return resources.Capacity{CPUs: []int{0, 1, 2, 3}, MemoryMB: 8192, AvailableMB: 1200}, nil
	}
	if _, err := d.Create(ctx, "pressure", spec); err == nil || !strings.Contains(err.Error(), "insufficient memory") {
		t.Fatalf("memory refusal: %v", err)
	}
	if len(f.requests) != before {
		t.Fatalf("admission failures sent requests: %v", f.requests[before:])
	}
	if entries, _ := os.ReadDir(d.root); len(entries) != 1 {
		t.Errorf("refused allocations left files: %v", entries)
	}
	if err := d.Remove(ctx, "default"); err != nil {
		t.Fatal(err)
	}
}

func TestAdoptLegacyResourceLimits(t *testing.T) {
	ctx := context.Background()
	f := newFakeEngine(t)
	d := f.driver(ctx, f.serve())
	if _, err := d.Create(ctx, "old", spec); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(d.root, "old", "resources.json")); err != nil {
		t.Fatal(err)
	}
	// Older containers had CPU quota but no cpuset.
	f.containers[d.name("old")].cfg.HostConfig.CpusetCpus = ""
	reopened, err := newDriver(d.e, d.root)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.adopt(ctx); err != nil {
		t.Fatal(err)
	}
	reopened.admission.Probe = func(string) (resources.Capacity, error) {
		return resources.Capacity{CPUs: []int{0, 1}, MemoryMB: 8192, AvailableMB: 8192}, nil
	}
	if _, err := reopened.Create(ctx, "new", spec); err == nil || !strings.Contains(err.Error(), "insufficient CPU") {
		t.Fatalf("legacy limit ignored: %v", err)
	}
	if f.containers[d.name("old")].cfg.HostConfig.CpusetCpus != "" {
		t.Error("adoption changed a legacy container")
	}
}

func TestRelocatedBackendKeepsNodeAccounting(t *testing.T) {
	f := newFakeEngine(t)
	d := f.driver(context.Background(), f.serve())
	state := shortTemp(t)
	relocated := shortTemp(t)
	if err := os.Symlink(relocated, filepath.Join(state, "docker")); err != nil {
		t.Fatal(err)
	}
	reopened, err := newDriver(d.e, filepath.Join(state, "docker"))
	if err != nil {
		t.Fatal(err)
	}
	if reopened.root != relocated || reopened.admission.StateDir != state {
		t.Fatalf("relocated backend lost node accounting: %s, %s", reopened.root, reopened.admission.StateDir)
	}
}

func TestRollbackRetainsResourceReservation(t *testing.T) {
	ctx := context.Background()
	f := newFakeEngine(t)
	d := f.driver(ctx, f.serve())
	name := d.name("broken")
	f.lose["POST /volumes/create"] = true
	f.fail["DELETE /volumes/"+name] = 500
	if _, err := d.Create(ctx, "broken", spec); err == nil {
		t.Fatal("failed create succeeded")
	}
	if _, err := os.Stat(filepath.Join(d.root, "broken", "resources.json")); err != nil {
		t.Fatalf("failed cleanup lost reservation: %v", err)
	}
	delete(f.fail, "DELETE /volumes/"+name)
	delete(f.lose, "POST /volumes/create")
	if err := d.Remove(ctx, "broken"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(d.root, "broken")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Remove did not release reservation: %v", err)
	}
	if _, err := d.Create(ctx, "broken", spec); err != nil {
		t.Fatal(err)
	}
}

func TestCreateRemoveKeepsReservationUntilCreateFinishes(t *testing.T) {
	ctx := context.Background()
	f := newFakeEngine(t)
	d := f.driver(ctx, f.serve())
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.hook = func(method, path string) {
		if method == "POST" && path == "/volumes/create" {
			once.Do(func() { close(entered); <-release })
		}
	}
	created := make(chan error, 1)
	go func() { _, err := d.Create(ctx, "pending", spec); created <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("create did not reach engine")
	}
	removeCtx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	err := d.Remove(removeCtx, "pending")
	_, statErr := os.Stat(filepath.Join(d.root, "pending", "resources.json"))
	close(release)
	createErr := <-created
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Remove bypassed pending Create: %v", err)
	}
	if statErr != nil {
		t.Errorf("pending reservation removed: %v", statErr)
	}
	if createErr != nil {
		t.Fatal(createErr)
	}
	if err := d.Remove(ctx, "pending"); err != nil {
		t.Fatal(err)
	}
}

func TestAdoptConcurrentRemove(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, phase := range []string{"inspection", "list"} {
			name := phase + "/current"
			if legacy {
				name = phase + "/legacy"
			}
			t.Run(name, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				f := newFakeEngine(t)
				d := f.driver(ctx, f.serve())
				if _, err := d.Create(ctx, "old", spec); err != nil {
					t.Fatal(err)
				}
				if legacy {
					if err := os.Remove(filepath.Join(d.root, "old", "resources.json")); err != nil {
						t.Fatal(err)
					}
				}
				other, err := newDriver(d.e, d.root)
				if err != nil {
					t.Fatal(err)
				}
				paused, release := make(chan struct{}), make(chan struct{})
				var held atomic.Bool
				path := "/containers/" + d.name("old") + "/json"
				if phase == "list" {
					path = "/containers/json"
				}
				f.hook = func(method, requestPath string) {
					if method == "GET" && requestPath == path && held.CompareAndSwap(false, true) {
						close(paused)
						<-release
					}
				}
				finished := make(chan struct{})
				var adoptErr error
				go func() { adoptErr = other.adopt(ctx); close(finished) }()
				var resume sync.Once
				defer func() {
					resume.Do(func() { close(release) })
					select {
					case <-finished:
					case <-time.After(5 * time.Second):
						t.Error("adoption did not finish after release")
					}
				}()
				select {
				case <-paused:
				case <-ctx.Done():
					t.Fatal("adoption never reached pause")
				}
				removeCtx := ctx
				if phase == "inspection" {
					var stop context.CancelFunc
					removeCtx, stop = context.WithTimeout(ctx, 100*time.Millisecond)
					defer stop()
				}
				removeErr := d.Remove(removeCtx, "old")
				if phase == "inspection" && !errors.Is(removeErr, context.DeadlineExceeded) {
					t.Errorf("Remove bypassed adoption's lifecycle lock: %v", removeErr)
				}
				if phase == "list" && removeErr != nil {
					t.Fatalf("Remove while List was paused: %v", removeErr)
				}
				resume.Do(func() { close(release) })
				select {
				case <-finished:
				case <-ctx.Done():
					t.Fatal("adoption did not finish")
				}
				if adoptErr != nil {
					t.Errorf("adoption after concurrent removal: %v", adoptErr)
				}
				if errors.Is(removeErr, context.DeadlineExceeded) {
					// The lock held Remove out; retry after adoption finishes.
					if err := d.Remove(ctx, "old"); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := os.Stat(filepath.Join(d.root, "old")); !os.IsNotExist(err) {
					t.Errorf("adoption resurrected removed reservation: %v", err)
				}
				d.admission.Probe = func(string) (resources.Capacity, error) {
					return resources.Capacity{CPUs: []int{0, 1}, MemoryMB: 8192, AvailableMB: 8192}, nil
				}
				if _, err := d.Create(ctx, "next", spec); err != nil {
					t.Errorf("removed sandbox still consumes capacity: %v", err)
				}
			})
		}
	}
}
