//go:build linux

package firecracker

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"boxpool.run/boxpool/internal/agent"
	"boxpool.run/boxpool/internal/artifacts"
	"boxpool.run/boxpool/internal/backend"
	"boxpool.run/boxpool/internal/resources"
)

func TestCreateListRemove(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	base := filepath.Join(dir, "rootfs.ext4")
	os.WriteFile(base, []byte("ext4"), 0644)
	d := &Driver{root: filepath.Join(dir, "sandboxes"), rootfs: base, admission: &resources.Manager{StateDir: dir, Probe: func(string) (resources.Capacity, error) {
		return resources.Capacity{CPUs: []int{0, 1, 2, 3}, MemoryMB: 8192, AvailableMB: 8192, DiskBytes: 1 << 40}, nil
	}}}
	oldResize := resizeFilesystem
	resizeFilesystem = func(context.Context, string) error { return nil }
	t.Cleanup(func() { resizeFilesystem = oldResize })
	os.Mkdir(d.root, 0700)
	spec := backend.Spec{Name: "web", CPUs: 1, MemoryMB: 256, DiskGB: 1}

	sb, err := d.Create(ctx, "abc", spec)
	if err != nil {
		t.Fatal(err)
	}
	if want := (backend.Sandbox{ID: "abc", Name: "web", State: backend.StateStopped}); sb != want {
		t.Errorf("Create returned %+v, want %+v", sb, want)
	}
	l := sandboxLayout(d.root, "abc")
	f, _ := os.Open(l.rootfs)
	b := make([]byte, 4)
	f.Read(b)
	f.Close()
	if string(b) != "ext4" {
		t.Errorf("rootfs copy starts %q", b)
	}
	if fi, err := os.Stat(l.rootfs); err != nil || fi.Size() != 1<<30 {
		t.Errorf("disk not resized: %v, %v", fi, err)
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
	if _, err := d.Create(ctx, "nocpu", backend.Spec{CPUs: -1}); err == nil {
		t.Error("Create with negative CPUs succeeded")
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

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := d.Remove(canceled, "abc"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(l.dir); !os.IsNotExist(err) {
		t.Errorf("Remove left %s: %v", l.dir, err)
	}
	if err := d.Remove(ctx, "abc"); err != nil {
		t.Errorf("second Remove: %v", err)
	}
}

// fakeFirecracker starts a process with args on its command line and writes
// sandbox id's runtime files for it, as Start would.
func fakeFirecracker(t *testing.T, root, id string, args ...string) (*exec.Cmd, layout) {
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

// assertKilled checks that cmd was killed and its runtime files removed.
func assertKilled(t *testing.T, exited <-chan error, l layout) {
	t.Helper()
	select {
	case err := <-exited:
		if err == nil {
			t.Error("orphan exited by itself")
		}
	case <-time.After(5 * time.Second):
		t.Error("orphan was not killed")
	}
	for _, p := range []string{l.pid, l.api, l.vsock} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s left behind: %v", p, err)
		}
	}
}

// vsockListener answers Firecracker's vsock CONNECT handshake before it
// hands a connection to the agent.
type vsockListener struct{ net.Listener }

func (l vsockListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	// The client sends nothing more until it reads OK, so this buffers only
	// the CONNECT line.
	if _, err := bufio.NewReader(c).ReadString('\n'); err != nil {
		c.Close()
		return l.Accept()
	}
	_, err = fmt.Fprintf(c, "OK %d\n", agentPort)
	return c, err
}

// fakeAgent serves a guest agent on l's vsock socket.
func fakeAgent(t *testing.T, l layout) {
	t.Helper()
	os.Remove(l.vsock)
	ln, err := agent.Listen("unix:" + l.vsock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- agent.Serve(ctx, vsockListener{ln}, "test") }()
	t.Cleanup(func() { cancel(); <-served })
}

func TestAdopt(t *testing.T) {
	root := t.TempDir()
	d := &Driver{root: root}
	// Its agent answers: a booted VM, adopted.
	booted, bl := fakeFirecracker(t, root, "booted", "--api-sock", sandboxLayout(root, "booted").api)
	fakeAgent(t, bl)
	// No agent, as when the daemon died while the VM booted: killed.
	orphan, ol := fakeFirecracker(t, root, "orphan", "--api-sock", sandboxLayout(root, "orphan").api)
	// A pid file naming a process that is not this sandbox's firecracker,
	// as after the pid was reused.
	other, _ := fakeFirecracker(t, root, "reused", "--api-sock", "/elsewhere/fc.sock")
	exited := make(chan error, 1)
	go func() { exited <- orphan.Wait() }()

	if err := d.adopt(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertKilled(t, exited, ol)
	// A killed child would be a zombie with an empty command line.
	if b, _ := os.ReadFile("/proc/" + strconv.Itoa(other.Process.Pid) + "/cmdline"); len(b) == 0 {
		t.Error("unrelated process was killed")
	}
	if _, ok := running(bl); !ok {
		t.Fatal("booted VM was not adopted")
	}
	for _, p := range []string{bl.pid, bl.api, bl.vsock} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("adopted VM lost %s: %v", p, err)
		}
	}
	if _, ok := d.consoles.Load("booted"); !ok {
		t.Fatal("adopted VM has no console rotator")
	}
	// Its console rotator ends once it is gone.
	booted.Process.Kill()
	booted.Wait()
	done := make(chan struct{})
	go func() { d.waitConsole("booted"); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("console rotator of the adopted VM did not end after it exited")
	}
}

func TestAdoptInterruptedProbe(t *testing.T) {
	for _, mode := range []string{"cancelled", "cancel-during-ping", "retry-agent"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			d := &Driver{root: root}
			vm, l := fakeFirecracker(t, root, "vm", "--api-sock", sandboxLayout(root, "vm").api)
			t.Cleanup(func() {
				vm.Process.Kill()
				vm.Wait()
				d.waitConsole("vm")
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "cancelled" {
				fakeAgent(t, l)
				if err := d.ping(ctx, l); err != nil {
					t.Fatal(err)
				}
				cancel()
			} else {
				// Hold the first connection so the test can cancel an active
				// probe or fail it before making a real agent available.
				if err := os.Remove(l.vsock); err != nil {
					t.Fatal(err)
				}
			}
			var ln *net.UnixListener
			if mode != "cancelled" {
				var err error
				ln, err = net.ListenUnix("unix", &net.UnixAddr{Name: l.vsock, Net: "unix"})
				if err != nil {
					t.Fatal(err)
				}
				defer ln.Close()
				ln.SetUnlinkOnClose(false)
				ln.SetDeadline(time.Now().Add(5 * time.Second))
			}
			done := make(chan error, 1)
			go func() { done <- d.adopt(ctx) }()
			if ln != nil {
				c, err := ln.Accept()
				if err != nil {
					t.Fatal(err)
				}
				if mode == "cancel-during-ping" {
					cancel()
				}
				c.Close()
				ln.Close()
				if mode == "retry-agent" {
					fakeAgent(t, l)
				}
			}
			select {
			case err := <-done:
				if mode == "retry-agent" {
					if err != nil {
						t.Fatal(err)
					}
				} else if !errors.Is(err, context.Canceled) {
					t.Fatalf("adopt = %v, want context.Canceled", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("adoption did not finish")
			}
			if err := waitExit(context.Background(), l, 50*time.Millisecond); err == nil {
				t.Fatal("adoption killed the VM")
			}
			for _, p := range []string{l.pid, l.api, l.vsock} {
				if _, err := os.Stat(p); err != nil {
					t.Errorf("adoption removed %s: %v", p, err)
				}
			}
		})
	}
}

// A VM started through the real state directory is an orphan to a Driver
// made through a symlink to it.
func TestNewThroughSymlink(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "sandboxes")
	os.Mkdir(root, 0700)
	orphan, ol := fakeFirecracker(t, root, "orphan", "--api-sock", sandboxLayout(root, "orphan").api)
	exited := make(chan error, 1)
	go func() { exited <- orphan.Wait() }()
	link := filepath.Join(t.TempDir(), "state")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	// Unpacked artifacts that are present, so prepare keeps them.
	art := t.TempDir()
	paths := artifacts.Paths{Firecracker: filepath.Join(art, "fc.tgz"), Kernel: filepath.Join(art, "vmlinux"), Rootfs: filepath.Join(art, "rootfs.ext4.zst")}
	os.WriteFile(filepath.Join(art, "fc"), nil, 0755)
	os.WriteFile(filepath.Join(art, "rootfs.ext4"), nil, 0644)

	d, err := New(context.Background(), link, paths, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d.root != root {
		t.Errorf("root is %s, want %s", d.root, root)
	}
	if d.admission.StateDir != dir {
		t.Errorf("state alias did not share resource accounting: %s", d.admission.StateDir)
	}
	assertKilled(t, exited, ol)
}

// TestPoll checks that poll trusts the exited channel, not /proc: a just
// started process can look gone there (#26).
func TestPoll(t *testing.T) {
	ctx := context.Background()
	tries := 0
	err := poll(ctx, make(chan struct{}), time.Second, "x", func(context.Context) error {
		if tries++; tries < 3 {
			return errors.New("not yet")
		}
		return nil
	})
	if err != nil || tries != 3 {
		t.Errorf("poll = %v after %d tries, want success after 3", err, tries)
	}
	exited := make(chan struct{})
	close(exited)
	if err := poll(ctx, exited, time.Second, "x", func(context.Context) error { return errors.New("no") }); err == nil || err.Error() != "firecracker exited while waiting for x" {
		t.Errorf("poll after exit = %v", err)
	}
}
