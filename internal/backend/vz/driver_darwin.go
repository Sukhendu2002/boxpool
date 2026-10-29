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
	"unsafe"

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
	machines             sync.Map     // sandbox ID → *machine, while its VM may run
	lifetime             sync.RWMutex // Start holds a reader before taking a sandbox lock
	closed               bool         // protected by lifetime
	isolated             bool         // sandboxes get networks of their own; without them, no network
	networks             sync.Map     // sandbox ID → its vmnet network, from its first Start until Remove
	paths                artifacts.Paths
	warm                 *layout // immutable default-size base
}

var _ backend.Driver = (*Driver)(nil)

// machine is one sandbox's running VM.
type machine struct {
	vm interface {
		CanRequestStop() bool
		RequestStop() (bool, error)
		CanStop() bool
		Stop() error
	}
	vsock interface {
		Connect(uint32) (*virt.VirtioSocketConnection, error)
	}
	saveRestoreErr error         // non-nil when this VM cannot use saved machine state
	stopped        chan struct{} // closed once the VM has stopped
	console        chan struct{} // closed once the console rotator has returned
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
		isolated:  IsolatedNetworks(),
		paths:     paths,
	}, nil
}

func (d *Driver) Name() string { return "vz" }

// Create reserves capacity and makes the sandbox's disk an APFS clone of the
// base rootfs, or of the disk snapshot Spec.From, sized to Spec.DiskGB GiB; Start grows the file system into
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
	src := d.rootfs
	warm := d.warm != nil && backend.WarmSpec(spec)
	if spec.From != "" {
		src = spec.From
	} else if warm {
		src = d.warm.rootfs
	}
	// clonefile keeps the base image shared until the guest writes.
	err = unix.Clonefile(src, l.rootfs, 0)
	if err == nil && warm {
		for _, pair := range [][2]string{{d.warm.state, l.state}, {d.warm.machineID, l.machineID}} {
			if err = unix.Clonefile(pair[0], pair[1], 0); err != nil {
				break
			}
		}
		if err == nil {
			// Saved state requires the same virtual device identifiers.
			err = os.WriteFile(filepath.Join(l.dir, "warm-mac"), []byte(macAddress(backend.WarmID)), 0600)
		}
	}
	if err == nil && !warm {
		err = growDisk(l.rootfs, int64(spec.DiskGB)<<30)
	}
	if err == nil {
		err = os.WriteFile(filepath.Join(l.dir, "fresh"), nil, 0600)
	}
	if err == nil {
		// Written last: List and Start ignore a directory without it.
		err = writeSpec(l, b)
	}
	if err != nil {
		return backend.Sandbox{}, fmt.Errorf("create sandbox %s: %w", id, errors.Join(err, os.RemoveAll(l.dir)))
	}
	return backend.Sandbox{ID: id, Name: spec.Name, State: backend.StateStopped}, nil
}

// writeSpec publishes the complete marker atomically, never an empty/partial file.
func writeSpec(l layout, b []byte) (err error) {
	f, err := os.CreateTemp(l.dir, ".spec-*")
	if err != nil {
		return err
	}
	defer func() {
		if cleanup := os.Remove(f.Name()); !errors.Is(cleanup, fs.ErrNotExist) {
			err = errors.Join(err, cleanup)
		}
	}()
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), l.spec)
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

// Snapshot makes path an APFS clone of a stopped sandbox's disk. A running
// or paused sandbox is refused: its disk may lag what the guest has written.
func (d *Driver) Snapshot(ctx context.Context, id, path string) error {
	if err := validID(id); err != nil {
		return err
	}
	unlock, err := d.admission.LockSandbox(ctx, "vz", id)
	if err != nil {
		return err
	}
	defer unlock()
	l, _, err := d.load(id)
	if err != nil {
		return err
	}
	if d.running(id) != nil || savedState(l) {
		return fmt.Errorf("snapshot sandbox %s: it is not stopped", id)
	}
	if err := unix.Clonefile(l.rootfs, path, 0); err != nil {
		return fmt.Errorf("snapshot sandbox %s: %w", id, err)
	}
	return nil
}

// Start boots the sandbox, or restores a saved machine state, and returns
// once its agent answers. Starting a running sandbox does nothing.
func (d *Driver) Start(ctx context.Context, id string) error {
	return d.start(ctx, id, false)
}

// Resume restores a paused sandbox and returns once its agent answers.
// Resuming a running sandbox does nothing.
func (d *Driver) Resume(ctx context.Context, id string) error {
	return d.start(ctx, id, true)
}

func (d *Driver) start(ctx context.Context, id string, resume bool) error {
	if err := validID(id); err != nil {
		return err
	}
	d.lifetime.RLock()
	defer d.lifetime.RUnlock()
	if d.closed {
		return errors.New("the vz driver is closed")
	}
	unlock, err := d.admission.LockSandbox(ctx, "vz", id)
	if err != nil {
		return err
	}
	defer unlock()
	return d.startLocked(ctx, id, resume)
}

// startLocked starts or restores a sandbox with its admission lock held.
func (d *Driver) startLocked(ctx context.Context, id string, resume bool) error {
	l, spec, err := d.load(id)
	if err != nil {
		return err
	}
	if d.running(id) != nil {
		return nil
	}
	restore := savedState(l)
	if resume && !restore {
		return fmt.Errorf("resume sandbox %s: it is not paused", id)
	}
	d.waitConsole(id)
	cfg, err := newVMConfig(spec, d.kernel, l, d.limits)
	if err != nil {
		return err
	}
	warmClone := false
	if mac, err := os.ReadFile(filepath.Join(l.dir, "warm-mac")); err == nil {
		cfg.MAC = string(mac)
		warmClone = true
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	_, freshErr := os.Stat(filepath.Join(l.dir, "fresh"))
	fresh := freshErr == nil
	if freshErr != nil && !errors.Is(freshErr, fs.ErrNotExist) {
		return freshErr
	}
	if err := os.WriteFile(l.console, nil, 0600); err != nil {
		return err
	}
	var network unsafe.Pointer
	if d.isolated {
		if network, err = d.network(id); err != nil {
			return fmt.Errorf("start sandbox %s: %w", id, err)
		}
		cfg.CommandLine += netBootArg
	}
	var statePath string
	if restore {
		statePath = l.state
	}
	m, err := startVM(cfg, network, statePath)
	if err != nil {
		return fmt.Errorf("start sandbox %s: %w", id, err)
	}
	bootErr := d.boot(ctx, m)
	if bootErr == nil {
		var netConfig backend.GuestNetwork
		if warmClone && restore && d.isolated {
			netConfig, bootErr = networkConfig(network)
		}
		if bootErr == nil {
			ag := m.agent()
			bootErr = backend.ConfigureGuest(ctx, ag, id, spec.Name, fresh, restore, netConfig)
			ag.CloseIdleConnections()
		}
		if bootErr == nil && fresh {
			bootErr = os.Remove(filepath.Join(l.dir, "fresh"))
		}
	}
	if err := bootErr; err != nil {
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

// network returns the sandbox's vmnet network, taking one on its first
// Start. A sandbox keeps its network, and so its subnet and address, until
// Remove: handing a stopped sandbox's network to another would let that one
// take over its address and receive traffic meant for it.
func (d *Driver) network(id string) (unsafe.Pointer, error) {
	if v, ok := d.networks.Load(id); ok {
		return v.(unsafe.Pointer), nil
	}
	network, err := takeNetwork()
	if err != nil {
		return nil, err
	}
	d.networks.Store(id, network)
	return network, nil
}

// releaseNetwork returns a removed sandbox's vmnet network, if it has one.
func (d *Driver) releaseNetwork(id string) {
	if v, ok := d.networks.LoadAndDelete(id); ok {
		putNetwork(v.(unsafe.Pointer))
	}
}

// startVM builds the framework's objects for cfg and starts the VM, with a
// network device on network unless it is nil.
func startVM(cfg vmConfig, network unsafe.Pointer, statePath string) (*machine, error) {
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
	if err := blk.SetBlockDeviceIdentifier(cfg.MAC); err != nil && statePath != "" {
		return nil, err
	}
	c.SetStorageDevicesVirtualMachineConfiguration([]virt.StorageDeviceConfiguration{blk})
	capture, err := newConsoleCapture(cfg.Console)
	if err != nil {
		return nil, err
	}
	var vm *virt.VirtualMachine
	started := false
	defer func() {
		if !started {
			if vm != nil && vm.CanStop() {
				_ = vm.Stop()
			}
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
	// A vmnet network of its own keeps the VM from reaching other sandboxes
	// (see doc.go). The framework's shared NAT would not, so without one the
	// VM has no network device.
	if network != nil {
		nic, err := natNIC(cfg.MAC)
		if err != nil {
			return nil, err
		}
		if err := attachNetwork(nic, network); err != nil {
			return nil, err
		}
		c.SetNetworkDevicesVirtualMachineConfiguration([]*virt.VirtioNetworkDeviceConfiguration{nic})
	}
	machineID, err := loadMachineIdentifier(cfg.MachineID)
	if err != nil {
		return nil, fmt.Errorf("load machine identifier: %w", err)
	}
	platform, err := virt.NewGenericPlatformConfiguration(virt.WithGenericMachineIdentifier(machineID))
	if err != nil {
		return nil, err
	}
	c.SetPlatformVirtualMachineConfiguration(platform)
	if _, err := c.Validate(); err != nil {
		return nil, entitlementHint(err)
	}
	saveRestoreOK, saveRestoreErr := c.ValidateSaveRestoreSupport()
	if saveRestoreErr == nil && !saveRestoreOK {
		saveRestoreErr = errors.New("the VM configuration does not support saved machine state")
	}
	vm, err = virt.NewVirtualMachine(c)
	if err != nil {
		return nil, entitlementHint(err)
	}
	if statePath == "" {
		if err := vm.Start(); err != nil {
			return nil, entitlementHint(err)
		}
	} else {
		if saveRestoreErr != nil {
			return nil, fmt.Errorf("restore saved machine state: %w", saveRestoreErr)
		}
		if err := vm.RestoreMachineStateFromURL(statePath); err != nil {
			return nil, entitlementHint(err)
		}
		if err := vm.Resume(); err != nil {
			return nil, entitlementHint(err)
		}
		if err := os.Remove(statePath); err != nil {
			return nil, fmt.Errorf("remove restored machine state: %w", err)
		}
	}
	started = true
	m := &machine{vm: vm, vsock: vm.SocketDevices()[0], saveRestoreErr: saveRestoreErr, stopped: make(chan struct{}), console: make(chan struct{})}
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

// natNIC is a virtio network device on the framework's NAT, for
// attachNetwork to move onto a vmnet network.
func natNIC(mac string) (*virt.VirtioNetworkDeviceConfiguration, error) {
	hw, err := net.ParseMAC(mac)
	if err != nil {
		return nil, err
	}
	nat, err := virt.NewNATNetworkDeviceAttachment()
	if err != nil {
		return nil, fmt.Errorf("NAT network attachment: %w", err)
	}
	nic, err := virt.NewVirtioNetworkDeviceConfiguration(nat)
	if err != nil {
		return nil, err
	}
	addr, err := virt.NewMACAddress(hw)
	if err != nil {
		return nil, err
	}
	nic.SetMACAddress(addr)
	return nic, nil
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
	if err := run(ctx, ag, growFS); err != nil {
		return err
	}
	if !d.isolated {
		return nil
	}
	if err := run(ctx, ag, writeResolvConf); err != nil {
		return fmt.Errorf("the guest got no nameserver by DHCP from the NAT: %w", err)
	}
	return nil
}

// writeResolvConf points the guest's resolver at the nameserver the NAT's
// DHCP server gave the kernel. Every boot: the NAT's subnet can change.
const writeResolvConf = `grep '^nameserver ' /proc/net/pnp > /etc/resolv.conf`

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
	l, _, err := d.load(id)
	if err != nil {
		return err
	}
	if d.running(id) == nil && savedState(l) {
		// Restore first so dirty guest data held only in the saved VM state is
		// flushed by the normal clean shutdown path.
		if err := d.startLocked(lockCtx, id, true); err != nil {
			return err
		}
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
		slog.Warn("sandbox did not shut down cleanly; stopping it", "sandbox", id, "err", err)
		if requestErr := requestStop(m); requestErr != nil {
			if forceErr := forceStop(m); forceErr != nil {
				return fmt.Errorf("stop sandbox %s: %w", id, errors.Join(err, requestErr, forceErr))
			}
		}
		unclean = fmt.Errorf("stop sandbox %s: %w: %v", id, backend.ErrUncleanStop, err)
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
	// Only a sandbox that is really gone gives up its network: one still
	// listed must keep its address.
	if err := os.RemoveAll(sandboxLayout(d.root, id).dir); err != nil {
		return err
	}
	d.releaseNetwork(id)
	return nil
}

// Close stops every running sandbox, cleanly if it can, for a daemon that
// is exiting: its VMs would end with the process anyway, without a sync. It
// returns every stopped sandbox's network.
func (d *Driver) Close() error {
	// A waiting writer blocks new Starts; active Starts finish before the sweep.
	d.lifetime.Lock()
	defer d.lifetime.Unlock()
	d.closed = true
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
	d.networks.Range(func(key, _ any) bool {
		if d.running(key.(string)) == nil {
			d.releaseNetwork(key.(string))
		}
		return true
	})
	return errors.Join(errs...)
}

type pausableVM interface {
	CanPause() bool
	Pause() error
	Resume() error
	SaveMachineStateToPath(string) error
}

// Pause saves the VM's state and stops its process, so it uses no CPU or
// memory until Resume. If the host or VM configuration cannot save state, it
// stops the sandbox instead and tells the caller to use Start.
func (d *Driver) Pause(ctx context.Context, id string) error {
	if err := validID(id); err != nil {
		return err
	}
	unlock, err := d.admission.LockSandbox(ctx, "vz", id)
	if err != nil {
		return err
	}
	defer unlock()
	l, _, err := d.load(id)
	if err != nil {
		return err
	}
	if savedState(l) && d.running(id) == nil {
		return nil
	}
	m := d.running(id)
	if m == nil {
		return fmt.Errorf("pause sandbox %s: it is not running", id)
	}
	vm, ok := m.vm.(pausableVM)
	if !ok {
		return d.pauseFallback(ctx, id, errors.New("the VM does not support pausing"))
	}
	if m.saveRestoreErr != nil {
		return d.pauseFallback(ctx, id, m.saveRestoreErr)
	}
	if !vm.CanPause() {
		return d.pauseFallback(ctx, id, errors.New("the VM is not pausable"))
	}
	if err := vm.Pause(); err != nil {
		return fmt.Errorf("pause sandbox %s: %w", id, err)
	}
	if err := saveMachineState(l, vm); err != nil {
		resumeErr := vm.Resume()
		return fmt.Errorf("pause sandbox %s: %w", id, errors.Join(err, resumeErr))
	}
	if err := forceStop(m); err != nil {
		return fmt.Errorf("pause sandbox %s: saved machine state, but could not stop the VM: %w", id, err)
	}
	<-m.console
	d.machines.CompareAndDelete(id, m)
	return nil
}

func (d *Driver) pauseFallback(ctx context.Context, id string, reason error) error {
	if err := d.stop(ctx, id); err != nil {
		return fmt.Errorf("pause sandbox %s: saved machine state unsupported (%v); stop fallback failed: %w", id, reason, err)
	}
	return fmt.Errorf("pause sandbox %s: saved machine state unsupported (%v); stopped instead; use start to resume: %w", id, reason, backend.ErrNotSupported)
}

// saveMachineState publishes the state file only after the framework has
// written and synced it, so its presence always means a complete snapshot.
func saveMachineState(l layout, vm pausableVM) (err error) {
	f, err := os.CreateTemp(l.dir, ".machine-state-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if cleanup := os.Remove(tmp); !errors.Is(cleanup, fs.ErrNotExist) {
			err = errors.Join(err, cleanup)
		}
	}()
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Remove(tmp); err != nil {
		return err
	}
	if err = vm.SaveMachineStateToPath(tmp); err != nil {
		return err
	}
	if err = os.Chmod(tmp, 0600); err != nil {
		return err
	}
	if err = syncPath(tmp); err != nil {
		return err
	}
	if err = os.Rename(tmp, l.state); err != nil {
		return err
	}
	return syncPath(l.dir)
}

func savedState(l layout) bool {
	_, err := os.Stat(l.state)
	return err == nil
}

func removeSavedState(l layout) error {
	if err := os.Remove(l.state); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return syncPath(l.dir)
}

func syncPath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func loadMachineIdentifier(path string) (*virt.GenericMachineIdentifier, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		id, err := virt.NewGenericMachineIdentifier()
		if err != nil {
			return nil, err
		}
		if err := writeMachineIdentifier(path, id.DataRepresentation()); err != nil {
			return nil, err
		}
		return id, nil
	}
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return nil, errors.New("machine identifier is empty")
	}
	return virt.NewGenericMachineIdentifierWithData(b)
}

func writeMachineIdentifier(path string, data []byte) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), ".machine-id-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if cleanup := os.Remove(tmp); !errors.Is(cleanup, fs.ErrNotExist) {
			err = errors.Join(err, cleanup)
		}
	}()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Chmod(tmp, 0600); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	return syncPath(filepath.Dir(path))
}

// List returns the sandboxes on disk, running or stopped.
func (d *Driver) List(ctx context.Context) ([]backend.Sandbox, error) {
	entries, err := os.ReadDir(d.root)
	if err != nil {
		return nil, err
	}
	var list []backend.Sandbox
	for _, e := range entries {
		l, spec, err := d.load(e.Name())
		if errors.Is(err, fs.ErrNotExist) {
			continue // not a sandbox, or Create has not finished
		}
		if err != nil {
			return nil, err
		}
		state := backend.StateStopped
		if d.running(e.Name()) != nil {
			state = backend.StateRunning
		} else if savedState(l) {
			state = backend.StatePaused
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
