//go:build darwin && cgo

package vz

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unsafe"

	"boxpool.run/boxpool/internal/backend"
	"boxpool.run/boxpool/internal/resources"
)

func TestCreateListRemove(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	base := filepath.Join(dir, "base.ext4")
	if err := os.WriteFile(base, []byte("base"), 0600); err != nil {
		t.Fatal(err)
	}
	d := &Driver{root: filepath.Join(dir, "sandboxes"), rootfs: base, limits: testLimits, admission: &resources.Manager{
		StateDir: dir, Probe: func(string) (resources.Capacity, error) {
			return resources.Capacity{CPUs: []int{0, 1, 2, 3}, MemoryMB: 8192, AvailableMB: 8192, DiskBytes: 1 << 40}, nil
		},
	}}
	spec := backend.Spec{Name: "web", CPUs: 1, MemoryMB: 512, DiskGB: 1}
	sb, err := d.Create(ctx, "abc", spec)
	if err != nil {
		t.Fatal(err)
	}
	want := backend.Sandbox{ID: "abc", Name: "web", State: backend.StateStopped}
	if sb != want {
		t.Fatalf("Create: %+v, want %+v", sb, want)
	}
	l, got, err := d.load("abc")
	if err != nil || got != spec {
		t.Fatalf("load: %+v, %v", got, err)
	}
	if fi, err := os.Stat(l.rootfs); err != nil || fi.Size() != 1<<30 {
		t.Fatalf("rootfs size: %v, %v", fi, err)
	}
	f, err := os.OpenFile(l.rootfs, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteAt([]byte("mine"), 0)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(base); string(b) != "base" {
		t.Errorf("clone changed base: %q", b)
	}
	if _, err := d.Create(ctx, "abc", spec); err == nil {
		t.Error("duplicate Create succeeded")
	}
	for _, id := range []string{"", ".", "..", "a/b", "../x"} {
		if _, err := d.Create(ctx, id, spec); err == nil {
			t.Errorf("accepted ID %q", id)
		}
	}
	d.rootfs = filepath.Join(dir, "missing")
	if _, err := d.Create(ctx, "broken", spec); err == nil {
		t.Error("Create without base succeeded")
	}
	if _, err := os.Stat(filepath.Join(d.root, "broken")); !os.IsNotExist(err) {
		t.Errorf("failed Create kept reservation: %v", err)
	}
	os.Mkdir(filepath.Join(d.root, "partial"), 0700)
	// A fresh driver has only disk state; all previous VMs have ended.
	reopened := &Driver{root: d.root, admission: d.admission}
	if list, err := reopened.List(ctx); err != nil || !reflect.DeepEqual(list, []backend.Sandbox{want}) {
		t.Errorf("List after reopen: %+v, %v", list, err)
	}
	if err := d.Stop(ctx, "abc"); err != nil {
		t.Error(err)
	}
	if err := d.Pause(ctx, "abc"); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Errorf("Pause: %v", err)
	}
	if err := d.Resume(ctx, "abc"); err == nil || !strings.Contains(err.Error(), "not paused") {
		t.Errorf("Resume: %v", err)
	}
	if err := os.WriteFile(l.state, []byte("saved"), 0600); err != nil {
		t.Fatal(err)
	}
	if list, err := d.List(ctx); err != nil || len(list) != 1 || list[0].State != backend.StatePaused {
		t.Errorf("List with saved state: %+v, %v; want abc paused", list, err)
	}
	if err := d.Stop(ctx, "abc"); err == nil || !savedState(l) {
		t.Errorf("Stop of paused sandbox: %v; saved state was lost", err)
	}
	if _, err := d.Dial(ctx, "abc"); err == nil {
		t.Error("Dial of stopped sandbox succeeded")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := d.Remove(canceled, "abc"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(l.dir); !os.IsNotExist(err) {
		t.Errorf("Remove kept directory: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestGrowDiskRefusesShrink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rootfs")
	os.WriteFile(path, []byte("base"), 0600)
	if err := growDisk(path, 3); err == nil {
		t.Error("shrank disk below base size")
	}
}

func TestSaveMachineStatePublishesCompleteFile(t *testing.T) {
	l := sandboxLayout(t.TempDir(), "abc")
	if err := os.MkdirAll(l.dir, 0700); err != nil {
		t.Fatal(err)
	}
	vm := fakePausableVM{}
	if err := saveMachineState(l, vm); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(l.state); err != nil || string(got) != "state" {
		t.Errorf("saved state: %q, %v", got, err)
	}
	entries, err := os.ReadDir(l.dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(l.state) {
		t.Errorf("temporary state files: %v, %v", entries, err)
	}
}

type fakePausableVM struct{}

func (fakePausableVM) CanPause() bool { return true }
func (fakePausableVM) Pause() error   { return nil }
func (fakePausableVM) Resume() error  { return nil }
func (fakePausableVM) SaveMachineStateToPath(path string) error {
	return os.WriteFile(path, []byte("state"), 0600)
}

func TestEntitlementHint(t *testing.T) {
	err := errors.New("missing com.apple.security.virtualization entitlement")
	if got := entitlementHint(err); !errors.Is(got, err) || !strings.Contains(got.Error(), "scripts/dev-sign.sh") {
		t.Errorf("missing signing advice: %v", got)
	}
}

func TestRemoveKeepsNetworkWhenDeleteFails(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	base := filepath.Join(dir, "base.ext4")
	if err := os.WriteFile(base, []byte("base"), 0600); err != nil {
		t.Fatal(err)
	}
	d := &Driver{root: filepath.Join(dir, "sandboxes"), rootfs: base, limits: testLimits, admission: &resources.Manager{
		StateDir: dir, Probe: func(string) (resources.Capacity, error) {
			return resources.Capacity{CPUs: []int{0, 1, 2, 3}, MemoryMB: 8192, AvailableMB: 8192, DiskBytes: 1 << 40}, nil
		},
	}}
	if _, err := d.Create(ctx, "abc", backend.Spec{Name: "web", CPUs: 1, MemoryMB: 512, DiskGB: 1}); err != nil {
		t.Fatal(err)
	}
	var fake byte
	net := unsafe.Pointer(&fake)
	d.networks.Store("abc", net)
	idle := func() (n int) {
		networks.Lock()
		defer networks.Unlock()
		for _, i := range networks.idle {
			if i.net == net {
				n++
			}
		}
		return n
	}
	t.Cleanup(func() {
		networks.Lock()
		defer networks.Unlock()
		kept := networks.idle[:0]
		for _, i := range networks.idle {
			if i.net != net {
				kept = append(kept, i)
			}
		}
		networks.idle = kept
	})

	sandbox := sandboxLayout(d.root, "abc").dir
	if err := os.Chmod(sandbox, 0500); err != nil {
		t.Fatal(err)
	}
	if err := d.Remove(ctx, "abc"); err == nil {
		t.Fatal("Remove succeeded although the sandbox directory could not be deleted")
	}
	if v, ok := d.networks.Load("abc"); !ok || v.(unsafe.Pointer) != net {
		t.Error("a sandbox that failed to be removed lost its network")
	}
	if n := idle(); n != 0 {
		t.Errorf("network pooled %d times after a failed Remove", n)
	}

	if err := os.Chmod(sandbox, 0700); err != nil {
		t.Fatal(err)
	}
	if err := d.Remove(ctx, "abc"); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.networks.Load("abc"); ok {
		t.Error("a removed sandbox kept its network")
	}
	if n := idle(); n != 1 {
		t.Errorf("network pooled %d times after Remove, want 1", n)
	}
}
