//go:build darwin && cgo

package vz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	virt "github.com/Code-Hex/vz/v3"
	"golang.org/x/sys/unix"

	"boxpool.run/boxpool/internal/agent"
	"boxpool.run/boxpool/internal/artifacts"
	"boxpool.run/boxpool/internal/backend"
	"boxpool.run/boxpool/internal/resources"
)

// Timeouts; tests may shorten them.
var (
	agentTimeout = 30 * time.Second // VM start to the agent answering
	stopTimeout  = 10 * time.Second // each stage of Stop before the next, harsher one
)

// Driver is the Virtualization framework backend. VMs run inside this
// process, so they end with it; the sandbox directories hold everything
// else, and a new Driver sees an earlier one's sandboxes as stopped.
type Driver struct {
	root, kernel, rootfs string
	limits               limits
	admission            *resources.Manager
	machines             sync.Map // sandbox ID → *machine, while its VM may run
}

var _ backend.Driver = (*Driver)(nil)

// machine is one sandbox's running VM.
type machine struct {
	vm      *virt.VirtualMachine
	vsock   *virt.VirtioSocketDevice
	stopped chan struct{} // closed once the VM has stopped
	console chan struct{} // closed once the console rotator has returned
}

// New decompresses the fetched rootfs once and creates the sandboxes
// directory in stateDir.
func New(ctx context.Context, stateDir string, paths artifacts.Paths) (*Driver, error) {
	rootfs, err := artifacts.Rootfs(paths)
	if err != nil {
		return nil, err
	}
	root := filepath.Join(stateDir, "sandboxes")
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	return &Driver{
		root:   root,
		kernel: paths.Kernel,
		rootfs: rootfs,
		limits: limits{
			minCPUs:   virt.VirtualMachineConfigurationMinimumAllowedCPUCount(),
			maxCPUs:   virt.VirtualMachineConfigurationMaximumAllowedCPUCount(),
			minMemory: virt.VirtualMachineConfigurationMinimumAllowedMemorySize(),
			maxMemory: virt.VirtualMachineConfigurationMaximumAllowedMemorySize(),
		},
		admission: &resources.Manager{StateDir: stateDir},
	}, nil
}

func (d *Driver) Name() string { return "vz" }

// Create reserves capacity and makes the sandbox's disk an APFS clone of the
// base rootfs, sized to Spec.DiskGB GiB; Start grows the file system into
// it. Spec.Image is ignored (there is one image).
func (d *Driver) Create(ctx context.Context, id string, spec backend.Spec) (backend.Sandbox, error) {
	if err := validID(id); err != nil {
		return backend.Sandbox{}, err
	}
	var err error
	if spec, err = spec.WithDefaults(); err != nil {
		return backend.Sandbox{}, err
	}
	if _, err := newVMConfig(spec, d.kernel, layout{}, d.limits); err != nil {
		return backend.Sandbox{}, err
	}
	unlock, err := d.admission.LockSandbox(ctx, "vz", id)
	if err != nil {
		return backend.Sandbox{}, err
	}
	defer unlock()
	if _, err := d.admission.Create(ctx, "vz", id, spec, int64(spec.DiskGB)<<30); err != nil {
		return backend.Sandbox{}, err
	}
	l := sandboxLayout(d.root, id)
	b, _ := json.Marshal(spec)
	// clonefile keeps the base image shared until the guest writes.
	err = unix.Clonefile(d.rootfs, l.rootfs, 0)
	if err == nil {
		err = growDisk(l.rootfs, int64(spec.DiskGB)<<30)
	}
	if err == nil {
		// Written last: List and Start ignore a directory without it.
		err = os.WriteFile(l.spec, b, 0600)
	}
	if err != nil {
		return backend.Sandbox{}, fmt.Errorf("create sandbox %s: %w", id, errors.Join(err, os.RemoveAll(l.dir)))
	}
	return backend.Sandbox{ID: id, Name: spec.Name, State: backend.StateStopped}, nil
}

// growDisk extends the disk image sparsely; it never shrinks it.
func growDisk(path string, size int64) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if size < fi.Size() {
		return fmt.Errorf("requested disk is smaller than base image (%d bytes)", fi.Size())
	}
	return os.Truncate(path, size)
}

// Start boots the sandbox and returns once its agent answers. Starting a
// running sandbox does nothing.
func (d *Driver) Start(ctx context.Context, id string) error {
	if err := validID(id); err != nil {
		return err
	}
	unlock, err := d.admission.LockSandbox(ctx, "vz", id)
	if err != nil {
		return err
	}
	defer unlock()
	l, spec, err := d.load(id)
	if err != nil {
		return err
	}
	if d.running(id) != nil {
		return nil
	}
	d.waitConsole(id)
	cfg, err := newVMConfig(spec, d.kernel, l, d.limits)
	if err != nil {
		return err
	}
	if err := os.WriteFile(l.console, nil, 0600); err != nil {
		return err
	}
	m, err := startVM(cfg)
	if err != nil {
		return fmt.Errorf("start sandbox %s: %w", id, err)
	}
	if err := d.boot(ctx, m); err != nil {
		if serr := forceStop(m); serr != nil {
			d.machines.Store(id, m) // still running: Stop and Remove retry
			err = errors.Join(err, serr)
		} else {
			<-m.console
		}
		return fmt.Errorf("start sandbox %s: %w (see %s)", id, err, l.console)
	}
	d.machines.Store(id, m)
	go func() {
		<-m.stopped
		<-m.console
		d.machines.CompareAndDelete(id, m)
	}()
	return nil
}

// startVM builds the framework's objects for cfg and starts the VM.
func startVM(cfg vmConfig) (*machine, error) {
	boot, err := virt.NewLinuxBootLoader(cfg.Kernel, virt.WithCommandLine(cfg.CommandLine))
	if err != nil {
		return nil, err
	}
	c, err := virt.NewVirtualMachineConfiguration(boot, cfg.CPUs, cfg.MemoryBytes)
	if err != nil {
		return nil, err
	}
	disk, err := virt.NewDiskImageStorageDeviceAttachment(cfg.Disk, false)
	if err != nil {
		return nil, err
	}
	blk, err := virt.NewVirtioBlockDeviceConfiguration(disk)
	if err != nil {
		return nil, err
	}
	c.SetStorageDevicesVirtualMachineConfiguration([]virt.StorageDeviceConfiguration{blk})
	capture, err := newConsoleCapture(cfg.Console)
	if err != nil {
		return nil, err
	}
	started := false
	defer func() {
		if !started {
			capture.close()
		}
	}()
	out, err := virt.NewFileHandleSerialPortAttachment(capture.input, capture.output)
	if err != nil {
		return nil, err
	}
	console, err := virt.NewVirtioConsoleDeviceSerialPortConfiguration(out)
	if err != nil {
		return nil, err
	}
	c.SetSerialPortsVirtualMachineConfiguration([]*virt.VirtioConsoleDeviceSerialPortConfiguration{console})
	entropy, err := virt.NewVirtioEntropyDeviceConfiguration()
	if err != nil {
		return nil, err
	}
	c.SetEntropyDevicesVirtualMachineConfiguration([]*virt.VirtioEntropyDeviceConfiguration{entropy})
	vsock, err := virt.NewVirtioSocketDeviceConfiguration()
	if err != nil {
		return nil, err
	}
	c.SetSocketDevicesVirtualMachineConfiguration([]virt.SocketDeviceConfiguration{vsock})
	if _, err := c.Validate(); err != nil {
		return nil, entitlementHint(err)
	}
	vm, err := virt.NewVirtualMachine(c)
	if err != nil {
		return nil, entitlementHint(err)
	}
	if err := vm.Start(); err != nil {
		return nil, entitlementHint(err)
	}
	started = true
	m := &machine{vm: vm, vsock: vm.SocketDevices()[0], stopped: make(chan struct{}), console: make(chan struct{})}
	go func() {
		defer close(m.console)
		capture.run(m.stopped)
	}()
	go func() {
		// Queued from Start on, so the stop cannot be missed.
		for s := range vm.StateChangedNotify() {
			if s == virt.VirtualMachineStateStopped || s == virt.VirtualMachineStateError {
				close(m.stopped)
				return
			}
		}
	}()
	return m, nil
}

// entitlementHint explains the framework's error for a binary that was not
// signed with the virtualization entitlement.
func entitlementHint(err error) error {
	if strings.Contains(err.Error(), "com.apple.security.virtualization") {
		return fmt.Errorf("%w: sign the daemon binary with scripts/dev-sign.sh", err)
	}
	return err
}

func (d *Driver) boot(ctx context.Context, m *machine) error {
	ctx, cancel := context.WithTimeout(ctx, agentTimeout)
	defer cancel()
	ag := m.agent()
	defer ag.CloseIdleConnections()
	for {
		_, err := ag.Version(ctx)
		if err == nil {
			break
		}
		select {
		case <-m.stopped:
			return errors.New("the VM stopped while waiting for the guest agent")
		case <-ctx.Done():
			return fmt.Errorf("waiting for the guest agent: %w (last error: %v)", ctx.Err(), err)
		case <-time.After(5 * time.Millisecond):
		}
	}
	// The disk image is Spec.DiskGB GiB but the file system is the base
	// image's size until it is grown, online, so the host needs no
	// e2fsprogs. Every boot: a no-op once grown.
	return run(ctx, ag, growFS)
}

// growFS grows the guest's root file system to fill /dev/vda with the
// EXT4_IOC_RESIZE_FS ioctl. resize2fs cannot: it opens the device, which
// the pinned kernel refuses while it is mounted (BLK_DEV_WRITE_MOUNTED is
// off), and perl is in every Debian image.
const growFS = `blocks=$(( $(cat /sys/block/vda/size) * 512 / $(stat -f -c %S /) ))
exec perl -e 'open(my $f, "<", "/") or die "/: $!\n"; ioctl($f, 0x40086610, pack("Q", $ARGV[0])) or die "EXT4_IOC_RESIZE_FS: $!\n"' "$blocks"`

// run runs script in the guest and fails with its output unless it exits 0.
func run(ctx context.Context, ag *agent.Client, script string) error {
	var out bytes.Buffer
	s, err := ag.Exec(ctx, agent.ExecRequest{Argv: []string{"sh", "-c", script}}, &out, &out)
	if err != nil {
		return err
	}
	if code, err := s.Wait(); err != nil || code != 0 {
		return fmt.Errorf("%s: exit status %d, %v: %s", script, code, err, bytes.TrimSpace(out.Bytes()))
	}
	return nil
}

// Stop freezes the guest's disk, powers the guest off and waits for the VM
// to stop. If that fails, it asks the framework to stop the guest and then
// stops the VM by force, and returns an error wrapping
// backend.ErrUncleanStop: the sandbox is stopped, but its disk may be
// inconsistent.
func (d *Driver) Stop(ctx context.Context, id string) error {
	if err := validID(id); err != nil {
		return err
	}
	lockCtx, cancel := cleanupContext(ctx)
	defer cancel()
	unlock, err := d.admission.LockSandbox(lockCtx, "vz", id)
	if err != nil {
		return err
	}
	defer unlock()
	if _, _, err := d.load(id); err != nil {
		return err
	}
	return d.stop(ctx, id)
}

// stop is Stop with the sandbox lock held.
func (d *Driver) stop(ctx context.Context, id string) error {
	m := d.running(id)
	if m == nil {
		d.waitConsole(id)
		return nil
	}
	var unclean error
	if err := shutdownGuest(ctx, m); err != nil {
		unclean = fmt.Errorf("stop sandbox %s: %w: %v", id, backend.ErrUncleanStop, err)
		slog.Warn("sandbox did not shut down cleanly; stopping it", "sandbox", id, "err", err)
		if err := requestStop(m); err != nil {
			if err := forceStop(m); err != nil {
				return errors.Join(unclean, err)
			}
		}
	}
	<-m.console
	d.machines.CompareAndDelete(id, m)
	return unclean
}

func cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), stopTimeout)
}

// shutdownGuest flushes and freezes the guest's root file system, which
// leaves it consistent on disk with a clean journal, and powers the guest
// off with sysrq. Its PSCI SYSTEM_OFF stops the VM.
// shortcut: guest processes get no SIGTERM first, as in the Firecracker
// driver (the image has no poweroff command and tini is PID 1); #91 adds an
// agent shutdown endpoint for that.
func shutdownGuest(ctx context.Context, m *machine) error {
	ctx, cancel := context.WithTimeout(ctx, stopTimeout)
	defer cancel()
	ag := m.agent()
	defer ag.CloseIdleConnections()
	// Unlike a read-only remount, the freeze succeeds while guest processes
	// have files open for writing.
	if err := run(ctx, ag, "sync && fsfreeze --freeze /"); err != nil {
		return fmt.Errorf("freeze the guest's disk: %w", err)
	}
	// sysrq-trigger is on procfs, not the frozen disk, and sh is already
	// cached from the freeze. The guest powers off at once, so this command
	// never exits; keep ctx alive until the VM stops, since closing the
	// session could make the agent kill the command first.
	if _, err := ag.Exec(ctx, agent.ExecRequest{Argv: []string{"sh", "-c", "echo o > /proc/sysrq-trigger"}}, io.Discard, io.Discard); err != nil {
		return err
	}
	return waitStopped(ctx, m, stopTimeout)
}

// requestStop asks the guest to power off, as a power button would.
func requestStop(m *machine) error {
	if !m.vm.CanRequestStop() {
		return errors.New("the VM cannot take a stop request")
	}
	if _, err := m.vm.RequestStop(); err != nil {
		return err
	}
	return waitStopped(context.Background(), m, stopTimeout)
}

// forceStop stops the VM at once, like pulling its power cord.
func forceStop(m *machine) error {
	if m.vm.CanStop() {
		if err := m.vm.Stop(); err != nil {
			return err
		}
	}
	return waitStopped(context.Background(), m, stopTimeout)
}

func waitStopped(ctx context.Context, m *machine, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	select {
	case <-m.stopped:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("the VM did not stop: %w", ctx.Err())
	}
}

// Remove stops the sandbox by force if it runs and deletes its directory,
// which frees its reservation.
func (d *Driver) Remove(ctx context.Context, id string) error {
	if err := validID(id); err != nil {
		return err
	}
	cleanup, cancel := cleanupContext(ctx)
	defer cancel()
	unlock, err := d.admission.LockSandbox(cleanup, "vz", id)
	if err != nil {
		return err
	}
	defer unlock()
	if v, ok := d.machines.Load(id); ok {
		m := v.(*machine)
		if err := forceStop(m); err != nil {
			return err
		}
		<-m.console
		d.machines.CompareAndDelete(id, m)
	}
	return os.RemoveAll(sandboxLayout(d.root, id).dir)
}

// Close stops every running sandbox, cleanly if it can, for a daemon that
// is exiting: its VMs would end with the process anyway, without a sync.
func (d *Driver) Close() error {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	d.machines.Range(func(key, _ any) bool {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := d.Stop(context.Background(), key.(string)); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}()
		return true
	})
	wg.Wait()
	return errors.Join(errs...)
}

func (d *Driver) Pause(ctx context.Context, id string) error {
	return fmt.Errorf("pause: %w until saved machine state lands (#29)", backend.ErrNotSupported)
}

func (d *Driver) Resume(ctx context.Context, id string) error {
	return fmt.Errorf("resume: %w until saved machine state lands (#29)", backend.ErrNotSupported)
}

// List returns the sandboxes on disk, running or stopped.
func (d *Driver) List(ctx context.Context) ([]backend.Sandbox, error) {
	entries, err := os.ReadDir(d.root)
	if err != nil {
		return nil, err
	}
	var list []backend.Sandbox
	for _, e := range entries {
		_, spec, err := d.load(e.Name())
		if errors.Is(err, fs.ErrNotExist) {
			continue // not a sandbox, or Create has not finished
		}
		if err != nil {
			return nil, err
		}
		state := backend.StateStopped
		if d.running(e.Name()) != nil {
			state = backend.StateRunning
		}
		list = append(list, backend.Sandbox{ID: e.Name(), Name: spec.Name, State: state})
	}
	return list, nil
}

// Dial connects to the agent in a running sandbox, for agent.NewClient.
func (d *Driver) Dial(ctx context.Context, id string) (net.Conn, error) {
	if err := validID(id); err != nil {
		return nil, err
	}
	m := d.running(id)
	if m == nil {
		return nil, fmt.Errorf("sandbox %s is not running", id)
	}
	return m.dial(ctx)
}

// running returns the sandbox's machine unless its VM has stopped, e.g.
// because the guest panicked.
func (d *Driver) running(id string) *machine {
	v, ok := d.machines.Load(id)
	if !ok {
		return nil
	}
	m := v.(*machine)
	select {
	case <-m.stopped:
		return nil
	default:
		return m
	}
}

// waitConsole waits for output from an already stopped VM before its files
// are removed or reused by a new boot.
func (d *Driver) waitConsole(id string) {
	if v, ok := d.machines.Load(id); ok {
		<-v.(*machine).console
	}
}

func (m *machine) dial(ctx context.Context) (net.Conn, error) {
	type result struct {
		c   net.Conn
		err error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := m.vsock.Connect(agentPort)
		if err != nil {
			ch <- result{nil, err}
			return
		}
		ch <- result{c, nil}
	}()
	select {
	case r := <-ch:
		return r.c, r.err
	case <-ctx.Done():
		go func() {
			if r := <-ch; r.c != nil {
				r.c.Close()
			}
		}()
		return nil, ctx.Err()
	}
}

func (m *machine) agent() *agent.Client {
	return agent.NewClient(m.dial)
}

func (d *Driver) load(id string) (layout, backend.Spec, error) {
	var spec backend.Spec
	if err := validID(id); err != nil {
		return layout{}, spec, err
	}
	l := sandboxLayout(d.root, id)
	b, err := os.ReadFile(l.spec)
	if err != nil {
		return l, spec, fmt.Errorf("sandbox %s: %w", id, err)
	}
	return l, spec, json.Unmarshal(b, &spec)
}

func validID(id string) error {
	if id == "" || id == "." || id == ".." || filepath.Base(id) != id {
		return fmt.Errorf("invalid sandbox ID %q", id)
	}
	return nil
}
