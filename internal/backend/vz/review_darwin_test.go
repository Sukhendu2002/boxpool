//go:build darwin && cgo

package vz

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	virt "github.com/Code-Hex/vz/v3"

	"boxpool.run/boxpool/internal/backend"
	"boxpool.run/boxpool/internal/resources"
)

func reviewDriver(t *testing.T) *Driver {
	t.Helper()
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
	return d
}

// Stop must expose a hard error to the daemon while the VM is still alive.
func TestStopFailureIsNotUncleanStop(t *testing.T) {
	for _, forced := range []bool{false, true} {
		t.Run(map[bool]string{false: "force-fails", true: "force-succeeds"}[forced], func(t *testing.T) {
			d := reviewDriver(t)
			ctx := context.Background()
			if _, err := d.Create(ctx, "vm", backend.Spec{CPUs: 1, MemoryMB: 512, DiskGB: 1}); err != nil {
				t.Fatal(err)
			}
			m := &machine{stopped: make(chan struct{}), console: make(chan struct{})}
			guestErr := errors.New("guest unavailable")
			requestErr := errors.New("request stop failed")
			forceErr := errors.New("force stop failed")
			m.vsock = failedSocket{guestErr}
			m.vm = &stopVM{requestErr: requestErr, stop: func() error {
				if !forced {
					return forceErr
				}
				close(m.stopped)
				close(m.console)
				return nil
			}}
			d.machines.Store("vm", m)
			err := d.Stop(ctx, "vm")
			if err == nil || errors.Is(err, backend.ErrUncleanStop) != forced {
				t.Fatalf("Stop: %v; ErrUncleanStop = %v, want %v", err, errors.Is(err, backend.ErrUncleanStop), forced)
			}
			if !strings.Contains(err.Error(), guestErr.Error()) {
				t.Errorf("lost guest failure: %v", err)
			}
			if !forced {
				if !errors.Is(err, forceErr) || !errors.Is(err, requestErr) {
					t.Errorf("lost framework failures: %v", err)
				}
				if d.running("vm") != m {
					t.Error("failed Stop lost live VM")
				}
				list, err := d.List(ctx)
				if err != nil || len(list) != 1 || list[0].State != backend.StateRunning {
					t.Errorf("List after failed Stop: %v, %v", list, err)
				}
				close(m.stopped)
				close(m.console)
			} else if d.running("vm") != nil {
				t.Error("successful force Stop left VM running")
			}
		})
	}
}

type failedSocket struct{ err error }

func (s failedSocket) Connect(uint32) (*virt.VirtioSocketConnection, error) { return nil, s.err }

type stopVM struct {
	requestErr error
	stop       func() error
}

func (*stopVM) CanRequestStop() bool         { return true }
func (v *stopVM) RequestStop() (bool, error) { return false, v.requestErr }
func (*stopVM) CanStop() bool                { return true }
func (v *stopVM) Stop() error                { return v.stop() }

func TestCreatePublishesCompleteSpec(t *testing.T) {
	d := reviewDriver(t)
	ctx := context.Background()
	if err := os.MkdirAll(d.root, 0700); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	readErr := make(chan error, 1)
	go func() {
		defer close(readErr)
		for {
			select {
			case <-done:
				return
			default:
			}
			if _, err := d.List(ctx); err != nil {
				readErr <- err
				return
			}
		}
	}()
	// A large metadata field widens partial-write exposure without growing the disk.
	spec := backend.Spec{Name: "web", Image: strings.Repeat("x", 128<<10), CPUs: 1, MemoryMB: 512, DiskGB: 1}
	for i := 0; i < 100; i++ {
		if _, err := d.Create(ctx, "vm", spec); err != nil {
			t.Error(err)
			break
		}
		l, got, err := d.load("vm")
		if err != nil || got != spec {
			t.Errorf("published spec incomplete: %v", err)
			break
		}
		fi, err := os.Stat(l.spec)
		if err != nil || fi.Mode().Perm() != 0600 {
			t.Errorf("spec mode: %v, %v", fi, err)
			break
		}
		entries, err := os.ReadDir(l.dir)
		if err != nil {
			t.Error(err)
			break
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".spec-") {
				t.Errorf("left temporary spec %s", entry.Name())
			}
		}
		if err := d.Remove(ctx, "vm"); err != nil {
			t.Error(err)
			break
		}
	}
	close(done)
	if err := <-readErr; err != nil {
		t.Fatalf("List observed unpublished/partial spec: %v", err)
	}
	if b, err := os.ReadFile(d.rootfs); err != nil || string(b) != "base" {
		t.Errorf("changed base: %q, %v", b, err)
	}
	// No reservation remains after repeated create/remove.
	if _, err := d.Create(ctx, "vm", spec); err != nil {
		t.Fatal(err)
	}
}

func TestCloseWaitsForStart(t *testing.T) {
	for _, failStart := range []bool{false, true} {
		t.Run(map[bool]string{false: "running", true: "failed-start"}[failStart], func(t *testing.T) {
			d := reviewDriver(t)
			ctx := context.Background()
			if _, err := d.Create(ctx, "vm", backend.Spec{CPUs: 1, MemoryMB: 512, DiskGB: 1}); err != nil {
				t.Fatal(err)
			}
			unlock, err := d.admission.LockSandbox(ctx, "vz", "vm")
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if unlock != nil {
					unlock()
				}
			}()
			startCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			started := make(chan error, 1)
			go func() { started <- d.Start(startCtx, "vm") }()
			// Start is inside its lifetime gate, paused on the sandbox lock.
			waitFor(t, func() bool {
				if d.lifetime.TryLock() {
					d.lifetime.Unlock()
					return false
				}
				return true
			})
			if !d.lifetime.TryRLock() {
				t.Fatal("Start serialized unrelated Starts")
			}
			d.lifetime.RUnlock()
			closed := make(chan error, 1)
			go func() { closed <- d.Close() }()
			waitFor(t, func() bool {
				select {
				case err := <-closed:
					t.Fatalf("Close returned before Start: %v", err)
				default:
				}
				if d.lifetime.TryRLock() {
					d.lifetime.RUnlock()
					return false
				}
				return true
			})
			select {
			case err := <-closed:
				t.Fatalf("Close returned before Start: %v", err)
			default:
			}
			// A new Start cannot sneak in while Close is waiting for the first.
			late := make(chan error, 1)
			go func() { late <- d.Start(ctx, "vm") }()
			if failStart {
				cancel()
			} else {
				m := &machine{stopped: make(chan struct{}), console: make(chan struct{})}
				m.vsock = failedSocket{errors.New("guest unavailable")}
				m.vm = &stopVM{requestErr: errors.New("request failed"), stop: func() error {
					close(m.stopped)
					close(m.console)
					return nil
				}}
				d.machines.Store("vm", m)
			}
			unlock()
			unlock = nil
			if err := <-started; (err != nil) != failStart {
				t.Fatalf("Start: %v", err)
			}
			if err := <-closed; err != nil && !errors.Is(err, backend.ErrUncleanStop) {
				t.Fatal(err)
			}
			if err := <-late; err == nil || !strings.Contains(err.Error(), "closed") {
				t.Errorf("Start during Close: %v", err)
			}
			if d.running("vm") != nil {
				t.Error("Close missed the VM")
			}
			if err := d.Start(ctx, "vm"); err == nil || !strings.Contains(err.Error(), "closed") {
				t.Errorf("Start after Close: %v", err)
			}
			if err := d.Close(); err != nil {
				t.Errorf("second Close: %v", err)
			}
		})
	}
}

func TestWriteSpecFailureCleansTemporary(t *testing.T) {
	l := sandboxLayout(t.TempDir(), "vm")
	// A directory at the destination makes rename fail after write/sync/close.
	if err := os.MkdirAll(l.spec, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeSpec(l, []byte(`{}`)); err == nil {
		t.Fatal("published over a directory")
	}
	entries, err := os.ReadDir(l.dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(l.spec) {
		t.Fatalf("temporary leaked: %v, %v", entries, err)
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for lifecycle gate")
		}
		time.Sleep(time.Millisecond)
	}
}
