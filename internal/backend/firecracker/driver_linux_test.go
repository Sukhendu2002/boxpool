//go:build linux

package firecracker

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"

	"boxpool.run/boxpool/internal/backend"
)

func TestCreateListRemove(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	base := filepath.Join(dir, "rootfs.ext4")
	os.WriteFile(base, []byte("ext4"), 0644)
	d := &Driver{root: filepath.Join(dir, "sandboxes"), rootfs: base}
	os.Mkdir(d.root, 0700)
	spec := backend.Spec{Name: "web", CPUs: 1, MemoryMB: 256}

	sb, err := d.Create(ctx, "abc", spec)
	if err != nil {
		t.Fatal(err)
	}
	if want := (backend.Sandbox{ID: "abc", Name: "web", State: backend.StateStopped}); sb != want {
		t.Errorf("Create returned %+v, want %+v", sb, want)
	}
	l := sandboxLayout(d.root, "abc")
	if b, _ := os.ReadFile(l.rootfs); string(b) != "ext4" {
		t.Errorf("rootfs copy is %q", b)
	}
	if fi, err := os.Stat(l.dir); err != nil || fi.Mode().Perm() != 0700 {
		t.Errorf("sandbox dir: %v %v", fi.Mode(), err)
	}
	if _, got, err := d.load("abc"); err != nil || got != spec {
		t.Errorf("load: %+v %v, want %+v", got, err, spec)
	}

	if _, err := d.Create(ctx, "abc", spec); err == nil {
		t.Error("second Create with the same ID succeeded")
	}
	for _, id := range []string{"", ".", "..", "a/b", "../x"} {
		if _, err := d.Create(ctx, id, spec); err == nil {
			t.Errorf("Create(%q) succeeded", id)
		}
	}
	if _, err := d.Create(ctx, "nocpu", backend.Spec{MemoryMB: 256}); err == nil {
		t.Error("Create with 0 CPUs succeeded")
	}
	// A failed copy leaves no directory behind.
	d.rootfs = filepath.Join(dir, "missing")
	if _, err := d.Create(ctx, "broken", spec); err == nil {
		t.Error("Create without a base rootfs succeeded")
	}
	// A directory without spec.json is not a sandbox.
	os.Mkdir(filepath.Join(d.root, "partial"), 0700)

	list, err := d.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := []backend.Sandbox{sb}; !reflect.DeepEqual(list, want) {
		t.Errorf("List: got %+v, want %+v", list, want)
	}
	if err := d.Start(ctx, "missing"); err == nil {
		t.Error("Start of a missing sandbox succeeded")
	}
	if err := d.Stop(ctx, "abc"); err != nil {
		t.Errorf("Stop of a stopped sandbox: %v", err)
	}
	if err := d.Pause(ctx, "abc"); !errors.Is(err, backend.ErrNotSupported) {
		t.Errorf("Pause: %v", err)
	}

	if err := d.Remove(ctx, "abc"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(l.dir); !os.IsNotExist(err) {
		t.Errorf("Remove left %s: %v", l.dir, err)
	}
	if err := d.Remove(ctx, "abc"); err != nil {
		t.Errorf("second Remove: %v", err)
	}
}

func TestKillOrphans(t *testing.T) {
	root := t.TempDir()
	start := func(id string, args ...string) (*exec.Cmd, layout) {
		t.Helper()
		l := sandboxLayout(root, id)
		os.Mkdir(l.dir, 0700)
		// A loop, so sh does not exec sleep and keeps args on its command line.
		cmd := exec.Command("sh", append([]string{"-c", "while :; do sleep 1; done", "sh"}, args...)...)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
		os.WriteFile(l.pid, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0600)
		os.WriteFile(l.api, nil, 0600)
		os.WriteFile(l.vsock, nil, 0600)
		return cmd, l
	}
	orphan, ol := start("orphan", "--api-sock", sandboxLayout(root, "orphan").api)
	// A pid file naming a process that is not this sandbox's firecracker,
	// as after the pid was reused.
	other, _ := start("reused", "--api-sock", "/elsewhere/fc.sock")
	exited := make(chan error, 1)
	go func() { exited <- orphan.Wait() }()

	if err := killOrphans(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	if err := <-exited; err == nil {
		t.Error("orphan was not killed")
	}
	for _, p := range []string{ol.pid, ol.api, ol.vsock} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s left behind: %v", p, err)
		}
	}
	// A killed child would be a zombie with an empty command line.
	if b, _ := os.ReadFile("/proc/" + strconv.Itoa(other.Process.Pid) + "/cmdline"); len(b) == 0 {
		t.Error("unrelated process was killed")
	}
}
