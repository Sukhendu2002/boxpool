//go:build linux

package firecracker

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
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"boxpool.run/boxpool/internal/agent"
	"boxpool.run/boxpool/internal/artifacts"
	"boxpool.run/boxpool/internal/backend"
	"boxpool.run/boxpool/internal/resources"
	"boxpool.run/boxpool/internal/store"
)

// Timeouts; tests may shorten them.
var (
	apiTimeout   = 5 * time.Second  // firecracker start to its API socket accepting
	agentTimeout = 30 * time.Second // InstanceStart to the agent answering
	stopTimeout  = 10 * time.Second // each stage of Stop before the next, harsher one
)

// Driver is the Firecracker backend. All its state is in the sandbox
// directories, so a new Driver sees the sandboxes an earlier one created.
type Driver struct {
	root                        string // <state dir>/sandboxes, symlinks resolved
	firecracker, kernel, rootfs string
	consoles                    sync.Map     // sandbox ID → chan closed when its console rotator returns
	store                       *store.Store // network slots; nil without networking
	netMu                       sync.Mutex   // held while changing tap devices and firewall rules
	admission                   *resources.Manager
	paths                       artifacts.Paths
	warm                        *layout // immutable base disk and saved memory
	networkRoot                 string  // warm builder shares the node's network ownership
}

var _ backend.Driver = (*Driver)(nil)

// New unpacks the fetched artifacts, creates the sandboxes directory in
// stateDir and adopts the firecracker processes an earlier daemon left
// running (see adopt). With a store, sandboxes get a network (see the
// package doc), which needs root; the tap devices of VMs that are gone are
// removed, and the firewall rules once none is left. Without one, sandboxes
// have no network interface.
func New(ctx context.Context, stateDir string, paths artifacts.Paths, st *store.Store) (*Driver, error) {
	if st != nil {
		if err := checkNetwork(); err != nil {
			return nil, err
		}
	}
	fc, rootfs, err := prepare(paths)
	if err != nil {
		return nil, err
	}
	root := filepath.Join(stateDir, "sandboxes")
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	state, err := filepath.Abs(stateDir)
	if err == nil {
		state, err = filepath.EvalSymlinks(state)
	}
	if err != nil {
		return nil, err
	}
	// Absolute and without symlinks, because running matches the API socket
	// path on firecracker's command line: an alias of the state directory
	// would not see the VMs started through another.
	if root, err = filepath.Abs(root); err == nil {
		root, err = filepath.EvalSymlinks(root)
	}
	if err != nil {
		return nil, err
	}
	d := &Driver{root: root, firecracker: fc, kernel: paths.Kernel, rootfs: rootfs, store: st, admission: &resources.Manager{StateDir: state}, paths: paths}
	if err := d.adopt(ctx); err != nil || st == nil {
		return d, err
	}
	return d, d.sweepNetwork(ctx)
}

// adopt takes over the firecracker processes an earlier daemon started:
// VMs whose agent answers keep running, with their console bounded again.
// The rest, such as a VM the daemon died while booting, are killed, and
// the runtime files of VMs that are gone are removed.
func (d *Driver) adopt(ctx context.Context) error {
	entries, err := os.ReadDir(d.root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !e.IsDir() {
			continue
		}
		l := sandboxLayout(d.root, e.Name())
		if pid, ok := running(l); ok {
			err := backend.GuestReady(l.dir)
			if err == nil {
				err = d.ping(ctx, l)
			}
			if err == nil {
				slog.Info("adopting running sandbox", "sandbox", e.Name(), "pid", pid)
				d.watchConsole(e.Name(), l)
				continue
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			slog.Warn("killing firecracker process whose agent does not answer", "sandbox", e.Name(), "pid", pid, "err", err)
		}
		if err := kill(ctx, l); err != nil {
			return err
		}
	}
	return nil
}

// ping checks that the sandbox's agent answers within apiTimeout.
func (d *Driver) ping(ctx context.Context, l layout) error {
	ag := d.agent(l)
	defer ag.CloseIdleConnections()
	return poll(ctx, nil, apiTimeout, "the guest agent", func(ctx context.Context) error {
		_, err := ag.Version(ctx)
		return err
	})
}

// watchConsole bounds the console log of an adopted VM, which is not this
// process's child, until running reports it gone.
func (d *Driver) watchConsole(id string, l layout) {
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		for {
			if _, ok := running(l); !ok {
				return
			}
			time.Sleep(time.Second)
		}
	}()
	rotated := make(chan struct{})
	d.consoles.Store(id, rotated)
	go func() {
		defer close(rotated)
		boundConsole(l.console, exited)
	}()
}

func (d *Driver) Name() string { return "firecracker" }

func (d *Driver) lockSandbox(ctx context.Context, id string) (func(), error) {
	m := d.admission
	if m == nil {
		m = &resources.Manager{StateDir: filepath.Dir(d.root)}
	}
	return m.LockSandbox(ctx, "firecracker", id)
}

// Create reserves capacity and grows a private copy of the base rootfs, or
// of the disk snapshot Spec.From, to Spec.DiskGB GiB. Spec.Image is ignored
// (there is one image).
func (d *Driver) Create(ctx context.Context, id string, spec backend.Spec) (backend.Sandbox, error) {
	if err := validID(id); err != nil {
		return backend.Sandbox{}, err
	}
	var err error
	if spec, err = spec.WithDefaults(); err != nil {
		return backend.Sandbox{}, err
	}
	l := sandboxLayout(d.root, id)
	// sun_path holds 108 bytes including the terminating NUL.
	if len(l.vsock) > 107 {
		return backend.Sandbox{}, fmt.Errorf("sandbox %s: socket path %s is longer than 107 bytes: use a shorter state directory", id, l.vsock)
	}
	if d.admission == nil {
		return backend.Sandbox{}, fmt.Errorf("resource admission is not initialized")
	}
	unlock, err := d.lockSandbox(ctx, id)
	if err != nil {
		return backend.Sandbox{}, err
	}
	defer unlock()
	if _, err := d.admission.Create(ctx, "firecracker", id, spec, int64(spec.DiskGB)<<30); err != nil {
		return backend.Sandbox{}, err
	}
	b, _ := json.Marshal(spec)
	src := d.rootfs
	warm := d.warm != nil && backend.WarmSpec(spec)
	if spec.From != "" {
		src = spec.From
	} else if warm {
		src = d.warm.rootfs
	}
	err = copyRootfs(l.rootfs, src)
	if err == nil && warm {
		err = copyRootfs(l.mem, d.warm.mem)
		if err == nil {
			err = copyRootfs(l.vmstate, d.warm.vmstate)
		}
	}
	if err == nil && !warm {
		err = growRootfs(ctx, l.rootfs, int64(spec.DiskGB)<<30)
	}
	if err == nil {
		err = os.WriteFile(filepath.Join(l.dir, "fresh"), nil, 0600)
	}
	if err == nil {
		// Persist the readiness gate before publishing a restorable sandbox.
		err = syncPath(filepath.Join(l.dir, "fresh"))
	}
	if err == nil {
		err = syncPath(l.dir)
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

// Start boots the sandbox, or restores it if it is paused, and returns once
// its agent answers. Starting a running sandbox does nothing.
func (d *Driver) Start(ctx context.Context, id string) error { return d.start(ctx, id, false) }

// Resume restores a paused sandbox from its snapshot, which it deletes, and
// returns once its agent answers. Resuming a running sandbox does nothing.
func (d *Driver) Resume(ctx context.Context, id string) error { return d.start(ctx, id, true) }

func (d *Driver) start(ctx context.Context, id string, resume bool) error {
	if err := validID(id); err != nil {
		return err
	}
	unlock, err := d.lockSandbox(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	l, spec, err := d.load(id)
	if err != nil {
		return err
	}
	if _, ok := running(l); ok {
		return backend.GuestReady(l.dir)
	}
	if resume && !paused(l) {
		return fmt.Errorf("resume sandbox %s: it is not paused", id)
	}
	return d.launch(ctx, id, l, spec)
}

// launch starts the firecracker process of a sandbox that does not run and
// boots or restores it. The caller holds the sandbox's lock.
func (d *Driver) launch(ctx context.Context, id string, l layout, spec backend.Spec) (err error) {
	// Firecracker cannot bind over sockets left by an unclean exit.
	if err := removeRuntimeFiles(l); err != nil {
		return err
	}
	// Firecracker opens its log without creating it.
	if err := os.WriteFile(l.log, nil, 0600); err != nil {
		return err
	}
	var n *nic
	vmGone := true
	if d.store != nil {
		defer func() {
			if err != nil && vmGone {
				cleanup, cancel := cleanupContext(ctx)
				defer cancel()
				err = errors.Join(err, d.stopNetwork(cleanup, id))
			}
		}()
		if n, err = d.startNetwork(ctx, id); err != nil {
			return err
		}
	}
	// O_APPEND, so backend.RotateConsole can truncate it under firecracker.
	console, err := os.OpenFile(l.console, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	cmd := exec.Command(d.firecracker, "--api-sock", l.api, "--enable-pci", "--log-path", l.log, "--level", "Info")
	cmd.Stdout, cmd.Stderr = console, console
	// Its own session keeps a Ctrl-C in the daemon's terminal away from the
	// VM. No Pdeathsig: VMs outlive a daemon crash, and New adopts them.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err = cmd.Start()
	console.Close()
	if err != nil {
		return err
	}
	vmGone = false
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }() // reap it; running reads /proc, not cmd
	rotated := make(chan struct{})
	d.consoles.Store(id, rotated)
	go func() {
		defer close(rotated)
		boundConsole(l.console, exited)
	}()
	err = os.WriteFile(l.pid, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0600)
	if err == nil {
		err = d.boot(ctx, l, spec, n, exited)
	}
	if err != nil {
		cleanup, cancel := cleanupContext(ctx)
		defer cancel()
		killErr := cmd.Process.Kill()
		// Wait on the process we started, including when writing its pid
		// file failed. Never remove its network while it may still run.
		select {
		case <-exited:
			vmGone = true
			d.waitConsole(id)
			// "signal: killed" means it still ran when the start gave up.
			err = errors.Join(err, fmt.Errorf("firecracker: %s", cmd.ProcessState), removeRuntimeFiles(l))
		case <-cleanup.Done():
			err = errors.Join(err, fmt.Errorf("wait for failed start cleanup: %w", cleanup.Err()), killErr)
		}
		return fmt.Errorf("start sandbox %s: %w (see %s and %s)", id, err, l.log, l.console)
	}
	return nil
}

func apiClient(l layout) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", l.api)
		},
	}}
}

func (d *Driver) boot(ctx context.Context, l layout, spec backend.Spec, n *nic, exited <-chan struct{}) error {
	api := apiClient(l)
	defer api.CloseIdleConnections()
	err := poll(ctx, exited, apiTimeout, "the API socket", func(ctx context.Context) error {
		c, err := net.Dial("unix", l.api)
		if err == nil {
			c.Close()
		}
		return err
	})
	if err != nil {
		return err
	}
	restore := paused(l)
	_, freshErr := os.Stat(filepath.Join(l.dir, "fresh"))
	fresh := freshErr == nil
	if freshErr != nil && !errors.Is(freshErr, fs.ErrNotExist) {
		return freshErr
	}
	if restore {
		// Deleted before the guest runs on, because from then on its memory
		// and disk no longer match the snapshot.
		call := loadSnapshot(l)
		if fresh {
			body := call.body.(map[string]any)
			body["vsock_override"] = map[string]any{"uds_path": l.vsock}
			if n != nil {
				body["network_overrides"] = []map[string]any{{"iface_id": "eth0", "host_dev_name": n.tap}}
			}
		}
		err := send(ctx, api, http.MethodPut, call)
		if err == nil && fresh {
			// The snapshot initially opens the immutable base disk; redirect
			// it to the matching private clone before any vCPU can run.
			err = send(ctx, api, http.MethodPatch, apiCall{"/drives/rootfs", map[string]any{"drive_id": "rootfs", "path_on_host": l.rootfs}})
		}
		if err == nil {
			err = removeSnapshot(l)
		}
		if err == nil {
			err = send(ctx, api, http.MethodPatch, apiCall{"/vm", map[string]any{"state": "Resumed"}})
		}
		if err != nil {
			return err
		}
	} else {
		// A pause cut short by a daemon crash leaves files as large as the
		// guest's memory, which are useless without the state file.
		if err := removeSnapshot(l); err != nil {
			return err
		}
		for _, c := range apiCalls(spec, d.kernel, l, n) {
			if err := send(ctx, api, http.MethodPut, c); err != nil {
				return err
			}
		}
	}
	ag := d.agent(l)
	defer ag.CloseIdleConnections()
	err = poll(ctx, exited, agentTimeout, "the guest agent", func(ctx context.Context) error {
		_, err := ag.Version(ctx)
		return err
	})
	if err != nil {
		return err
	}
	if fresh && restore {
		if err := backend.FreezeGuest(ctx, ag, false); err != nil {
			return err
		}
	}
	var network backend.GuestNetwork
	if n != nil {
		network.Resolvers = n.resolvConf()
		if fresh && restore {
			network.Address, network.Mask, network.Gateway = n.guest.String(), "255.255.255.252", n.gateway.String()
		}
	}
	if err := backend.ConfigureGuest(ctx, ag, filepath.Base(l.dir), spec.Name, fresh, restore, network); err != nil {
		return err
	}
	if fresh {
		if err := os.Remove(filepath.Join(l.dir, "fresh")); err != nil {
			return err
		}
		return syncPath(l.dir)
	}
	return nil
}

func send(ctx context.Context, c *http.Client, method string, call apiCall) error {
	b, err := json.Marshal(call.body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://firecracker"+call.path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, call.path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("%s %s: %s: %s", method, call.path, resp.Status, bytes.TrimSpace(msg))
	}
	return nil
}

// poll calls try every few milliseconds until it succeeds, exited is closed
// or timeout passes. It does not use running: just after exec, until the
// kernel sets up the new program's arguments (milliseconds on a cold page
// cache, as after a reboot), /proc/<pid>/cmdline is empty, so a live
// process looks gone.
func poll(ctx context.Context, exited <-chan struct{}, timeout time.Duration, what string, try func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		err := try(ctx)
		if err == nil {
			return nil
		}
		select {
		case <-exited:
			return fmt.Errorf("firecracker exited while waiting for %s", what)
		case <-ctx.Done():
			return fmt.Errorf("waiting for %s: %w (last error: %v)", what, ctx.Err(), err)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// Stop freezes the guest's disk, reboots the guest and waits for firecracker
// to exit. If the freeze or the reboot fails, it sends SIGTERM and then
// SIGKILL instead, and, once firecracker is gone, returns an error wrapping
// backend.ErrUncleanStop: the sandbox is stopped, but its disk may be
// inconsistent.
func (d *Driver) Stop(ctx context.Context, id string) error {
	if err := validID(id); err != nil {
		return err
	}
	lockCtx, cancel := cleanupContext(ctx)
	defer cancel()
	unlock, err := d.lockSandbox(lockCtx, id)
	if err != nil {
		return err
	}
	defer unlock()
	l, spec, err := d.load(id)
	if err != nil {
		return err
	}
	var unclean error
	if _, ok := running(l); !ok && paused(l) {
		// Discarding the snapshot would lose what the guest had not yet
		// written to its disk, so restore it to shut it down cleanly. One
		// that cannot be restored must not block Stop and so Remove.
		if err := d.launch(ctx, id, l, spec); err != nil {
			unclean = fmt.Errorf("stop sandbox %s: %w: discarded its snapshot, which did not restore: %v", id, backend.ErrUncleanStop, err)
			slog.Warn("paused sandbox did not restore; discarding its snapshot", "sandbox", id, "err", err)
			if err := removeSnapshot(l); err != nil {
				return errors.Join(unclean, err)
			}
		}
	}
	if pid, ok := running(l); ok {
		if err := d.shutdownGuest(ctx, l); err != nil {
			unclean = fmt.Errorf("stop sandbox %s: %w: %v", id, backend.ErrUncleanStop, err)
			slog.Warn("sandbox did not shut down cleanly; terminating it", "sandbox", id, "err", err)
			syscall.Kill(pid, syscall.SIGTERM)
			if waitExit(ctx, l, stopTimeout) != nil {
				cleanup, cancel := cleanupContext(ctx)
				err := kill(cleanup, l)
				cancel()
				if err != nil {
					return err
				}
			}
		}
	}
	// The VM is confirmed gone. Caller cancellation must not strand its
	// network; cleanup still has a deadline of its own.
	cleanup, cancel := cleanupContext(ctx)
	defer cancel()
	d.waitConsole(id)
	return errors.Join(unclean, removeRuntimeFiles(l), d.stopNetwork(cleanup, id))
}

func cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), stopTimeout)
}

// shutdownGuest flushes and freezes the guest's root filesystem, which
// leaves it consistent on disk with a clean journal, and reboots the guest
// with sysrq. It then waits for firecracker to exit, which reboot=k (x86_64)
// or PSCI (arm64) makes it do.
// shortcut: guest processes get no SIGTERM first (writers block on the frozen
// filesystem and die with the VM), because SendCtrlAltDel needs an i8042
// driver the pinned kernel lacks (CONFIG_SERIO is off) and the image has no
// reboot command; add an agent shutdown endpoint for that.
func (d *Driver) shutdownGuest(ctx context.Context, l layout) error {
	ctx, cancel := context.WithTimeout(ctx, stopTimeout)
	defer cancel()
	c := d.agent(l)
	defer c.CloseIdleConnections()
	run := func(script string) (*agent.Session, error) {
		return c.Exec(ctx, agent.ExecRequest{Argv: []string{"sh", "-c", script}}, io.Discard, io.Discard)
	}
	// Unlike a read-only remount, the freeze succeeds while guest processes
	// have files open for writing.
	var out bytes.Buffer
	s, err := c.Exec(ctx, agent.ExecRequest{Argv: []string{"sh", "-c", "sync && fsfreeze --freeze /"}}, &out, &out)
	if err != nil {
		return err
	}
	if code, err := s.Wait(); err != nil || code != 0 {
		return fmt.Errorf("freeze the guest's disk: exit status %d, %v: %s", code, err, bytes.TrimSpace(out.Bytes()))
	}
	// sysrq-trigger is on procfs, not the frozen disk, and sh is already
	// cached from the freeze, so this runs. The guest reboots at once, so
	// this command never exits. Keep ctx alive
	// until firecracker does: cancelling it would close the session and could
	// make the agent kill the command first.
	if _, err := run("echo b > /proc/sysrq-trigger"); err != nil {
		return err
	}
	return waitExit(ctx, l, stopTimeout)
}

// Remove kills the sandbox if it runs, deletes its tap device and directory
// and frees its network slot.
func (d *Driver) Remove(ctx context.Context, id string) error {
	if err := validID(id); err != nil {
		return err
	}
	cleanup, cancel := cleanupContext(ctx)
	defer cancel()
	unlock, err := d.lockSandbox(cleanup, id)
	if err != nil {
		return err
	}
	defer unlock()
	l := sandboxLayout(d.root, id)
	if err := kill(cleanup, l); err != nil {
		return err
	}
	d.waitConsole(id)
	if err := d.stopNetwork(cleanup, id); err != nil {
		return err
	}
	if err := os.RemoveAll(l.dir); err != nil {
		return err
	}
	if d.store != nil {
		return d.store.FreeNetSlot(cleanup, id)
	}
	return nil
}

// Pause writes a full snapshot of the sandbox's memory and VM state to its
// directory, then ends its firecracker process and removes its tap device,
// so it uses no CPU or memory until Resume. Pausing a paused sandbox does
// nothing. If the snapshot fails, the guest runs on.
func (d *Driver) Pause(ctx context.Context, id string) error {
	if err := validID(id); err != nil {
		return err
	}
	unlock, err := d.lockSandbox(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	l, _, err := d.load(id)
	if err != nil {
		return err
	}
	if _, ok := running(l); !ok {
		if paused(l) {
			return nil
		}
		return fmt.Errorf("pause sandbox %s: it is not running", id)
	}
	api := apiClient(l)
	defer api.CloseIdleConnections()
	// Written under temporary names, synced with the disk, and renamed state
	// last: paused means the state file exists, so a pause cut short by an
	// error or a host crash never leaves a snapshot to restore.
	err = send(ctx, api, http.MethodPatch, apiCall{"/vm", map[string]any{"state": "Paused"}})
	if err == nil {
		err = send(ctx, api, http.MethodPut, apiCall{"/snapshot/create", map[string]any{
			"snapshot_type": "Full", "snapshot_path": l.vmstate + ".tmp", "mem_file_path": l.mem + ".tmp",
		}})
	}
	for _, p := range []string{l.mem + ".tmp", l.vmstate + ".tmp", l.rootfs} {
		if err == nil {
			err = syncPath(p)
		}
	}
	if err == nil {
		err = os.Rename(l.mem+".tmp", l.mem)
	}
	if err == nil {
		err = os.Rename(l.vmstate+".tmp", l.vmstate)
	}
	if err == nil {
		err = syncPath(l.dir)
	}
	if err != nil {
		cleanup, cancel := cleanupContext(ctx)
		defer cancel()
		resumeErr := send(cleanup, api, http.MethodPatch, apiCall{"/vm", map[string]any{"state": "Resumed"}})
		return fmt.Errorf("pause sandbox %s: %w", id, errors.Join(err, resumeErr, removeSnapshot(l)))
	}
	// The snapshot is complete: a caller giving up now must not leave the
	// paused VM and its network behind.
	cleanup, cancel := cleanupContext(ctx)
	defer cancel()
	if err := kill(cleanup, l); err != nil {
		return err
	}
	d.waitConsole(id)
	return d.stopNetwork(cleanup, id)
}

// Snapshot copies the disk of a stopped sandbox to path, sharing its blocks
// where the file system can. A running or paused sandbox is refused: its
// disk may lag what the guest has written.
func (d *Driver) Snapshot(ctx context.Context, id, path string) error {
	unlock, err := d.lockSandbox(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	l, _, err := d.load(id)
	if err != nil {
		return err
	}
	if _, ok := running(l); ok || paused(l) {
		return fmt.Errorf("snapshot sandbox %s: it is not stopped", id)
	}
	// copyRootfs removes a copy it fails to finish, and never an existing path.
	if err := copyRootfs(path, l.rootfs); err != nil {
		return fmt.Errorf("snapshot sandbox %s: %w", id, err)
	}
	if err := syncPath(path); err != nil {
		return fmt.Errorf("snapshot sandbox %s: %w", id, errors.Join(err, os.Remove(path)))
	}
	return nil
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
		if _, ok := running(l); ok {
			state = backend.StateRunning
		} else if paused(l) {
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
	l := sandboxLayout(d.root, id)
	if err := backend.GuestReady(l.dir); err != nil {
		return nil, err
	}
	return dialAgent(ctx, l.vsock)
}

// boundConsole rotates the console log at path once a second until exited
// is closed, which bounds it and its one rotated copy to about 2 MiB.
// shortcut: a guest writing faster than the serial port allows overshoots by
// a second's output, and output written during a rotation is lost; read the
// console through a pipe if that matters.
func boundConsole(path string, exited <-chan struct{}) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-exited:
			return
		case <-t.C:
			if err := backend.RotateConsole(path); err != nil {
				slog.Warn("rotating console log", "path", path, "err", err)
			}
		}
	}
}

// waitConsole waits for the console rotator of a sandbox whose firecracker
// process is gone, so it no longer touches the sandbox's files.
func (d *Driver) waitConsole(id string) {
	if done, ok := d.consoles.LoadAndDelete(id); ok {
		<-done.(chan struct{})
	}
}

func (d *Driver) agent(l layout) *agent.Client {
	return agent.NewClient(func(ctx context.Context) (net.Conn, error) { return dialAgent(ctx, l.vsock) })
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
	if id == "." || id == ".." || filepath.Base(id) != id {
		return fmt.Errorf("invalid sandbox ID %q", id)
	}
	return nil
}

// running returns the pid of the sandbox's firecracker process. A live pid
// is not enough: the process must also have this sandbox's API socket on its
// command line, which a process that reused the pid would not.
func running(l layout) (int, bool) {
	b, err := os.ReadFile(l.pid)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	// Empty for a zombie, so an exited but unreaped process counts as gone.
	cmdline, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return 0, false
	}
	for _, arg := range bytes.Split(cmdline, []byte{0}) {
		if string(arg) == l.api {
			return pid, true
		}
	}
	return 0, false
}

func waitExit(ctx context.Context, l layout, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		if _, ok := running(l); !ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("firecracker did not exit: %w", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// kill SIGKILLs the sandbox's firecracker process if it runs, waits for it
// to go and removes its sockets and pid file.
func kill(ctx context.Context, l layout) error {
	if pid, ok := running(l); ok {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
		if err := waitExit(ctx, l, stopTimeout); err != nil {
			return err
		}
	}
	return removeRuntimeFiles(l)
}

func removeRuntimeFiles(l layout) error {
	for _, p := range []string{l.pid, l.api, l.vsock} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// paused reports whether the sandbox has a snapshot to restore.
func paused(l layout) bool {
	_, err := os.Stat(l.vmstate)
	return err == nil
}

// removeSnapshot deletes the sandbox's snapshot and any partial one, state
// first, so the sandbox stops counting as paused before its memory goes.
func removeSnapshot(l layout) error {
	for _, p := range []string{l.vmstate, l.mem, l.vmstate + ".tmp", l.mem + ".tmp"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// syncPath flushes a file or directory to disk.
func syncPath(p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
