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
	if err := d.Pause(ctx, "abc"); !errors.Is(err, backend.ErrNotSupported) {
		t.Errorf("Pause: %v", err)
	}
	if err := d.Resume(ctx, "abc"); !errors.Is(err, backend.ErrNotSupported) {
		t.Errorf("Resume: %v", err)
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

func TestEntitlementHint(t *testing.T) {
	err := errors.New("missing com.apple.security.virtualization entitlement")
	if got := entitlementHint(err); !errors.Is(got, err) || !strings.Contains(got.Error(), "scripts/dev-sign.sh") {
		t.Errorf("missing signing advice: %v", got)
	}
}
