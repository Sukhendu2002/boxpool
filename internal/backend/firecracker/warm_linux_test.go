//go:build linux

package firecracker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"boxpool.run/boxpool/internal/backend"
	"boxpool.run/boxpool/internal/resources"
)

func TestCreateWarmClone(t *testing.T) {
	dir := t.TempDir()
	base := sandboxLayout(filepath.Join(dir, "cache"), "base")
	if err := os.MkdirAll(base.dir, 0700); err != nil {
		t.Fatal(err)
	}
	for path, value := range map[string]string{base.rootfs: "disk", base.vmstate: "state", base.mem: "memory"} {
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	d := &Driver{root: filepath.Join(dir, "sandboxes"), warm: &base,
		admission: &resources.Manager{StateDir: dir, Probe: func(string) (resources.Capacity, error) {
			return resources.Capacity{CPUs: []int{0, 1, 2, 3, 4, 5, 6}, MemoryMB: 16384, AvailableMB: 16384, DiskBytes: 1 << 40}, nil
		}}}
	if _, err := d.Create(context.Background(), "clone", backend.Spec{Name: "clone"}); err != nil {
		t.Fatal(err)
	}
	l := sandboxLayout(d.root, "clone")
	for path, want := range map[string]string{l.rootfs: "disk", l.vmstate: "state", l.mem: "memory"} {
		if got, err := os.ReadFile(path); err != nil || string(got) != want {
			t.Fatalf("%s: %q, %v", path, got, err)
		}
	}
	if err := os.WriteFile(l.rootfs, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(base.rootfs); string(got) != "disk" {
		t.Fatal("clone changed base")
	}
	if err := os.Remove(base.mem); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Create(context.Background(), "broken", backend.Spec{}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing memory: %v", err)
	}
	if _, err := os.Stat(sandboxLayout(d.root, "broken").dir); !os.IsNotExist(err) {
		t.Fatal("failed clone retained reservation")
	}
}
