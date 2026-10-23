package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"boxpool.run/boxpool/internal/client"
	"boxpool.run/boxpool/internal/preflight"
)

// Where boxpool up installs the daemon.
const (
	installPath = "/usr/local/bin/boxpool"
	unitPath    = "/etc/systemd/system/boxpool.service"
)

// upTimeout bounds the wait for a healthy daemon; its first start
// downloads about 60 MB.
const upTimeout = 5 * time.Minute

// unit renders the systemd unit that runs bin as the daemon.
//
// KillMode=process: stopping or restarting the unit signals only the
// daemon. Firecracker VMs run in their own sessions in this unit's cgroup;
// the default would SIGTERM and then SIGKILL them with it. They keep
// running until the daemon starts again, which today kills them as
// orphans; #27 will re-adopt them. Docker containers are not in this
// cgroup and survive either way. No User=: the daemon runs as root, which
// Firecracker networking needs.
func unit(bin string) []byte {
	return []byte(`[Unit]
Description=Boxpool node daemon
Documentation=https://github.com/Sukhendu2002/boxpool
After=network-online.target docker.service
Wants=network-online.target

[Service]
Type=simple
ExecStart=` + bin + ` daemon
Restart=on-failure
RestartSec=2
KillMode=process

[Install]
WantedBy=multi-user.target
`)
}

// systemdOnly returns false after printing why cmd cannot run here.
func systemdOnly(cmd string) bool {
	switch {
	case runtime.GOOS == "darwin":
		fmt.Fprintf(os.Stderr, "boxpool %s: not supported on macOS yet (#22); run boxpool daemon instead\n", cmd)
	case runtime.GOOS != "linux":
		fmt.Fprintf(os.Stderr, "boxpool %s: not supported on %s\n", cmd, runtime.GOOS)
	case os.Geteuid() != 0:
		fmt.Fprintf(os.Stderr, "boxpool %s: must run as root, to manage the systemd service: sudo boxpool %s\n", cmd, cmd)
	default:
		return true
	}
	return false
}

func up(args []string) int {
	if len(args) != 0 {
		fmt.Fprint(os.Stderr, "Usage: boxpool up\n\nInstalls this binary as "+installPath+" and runs it as the systemd\nservice boxpool, then waits until the daemon is healthy. Needs root.\n")
		return 2
	}
	if !systemdOnly("up") {
		return 1
	}
	os.Unsetenv("BOXPOOL_HOME") // the service runs without it
	if r := preflight.Run(); !r.Ready() {
		printReport(r)
		fmt.Fprintln(os.Stderr, "\nboxpool up: this machine cannot host sandboxes yet; fix the checks marked ✗ above")
		return 1
	}
	start := time.Now()
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	var bin []byte
	if err == nil {
		bin, err = os.ReadFile(exe)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "boxpool up: read this binary: %v\n", err)
		return 1
	}
	binChanged, err := installFile(installPath, bin, 0755)
	if err != nil {
		fmt.Fprintf(os.Stderr, "boxpool up: %v\n", err)
		return 1
	}
	unitChanged, err := installFile(unitPath, unit(installPath), 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "boxpool up: %v\n", err)
		return 1
	}
	report := func(changed bool, path string) {
		if changed {
			fmt.Printf("installed %s\n", path)
		} else {
			fmt.Printf("%s is up to date\n", path)
		}
	}
	report(binChanged, installPath)
	report(unitChanged, unitPath)

	facts, err := observe(unitChanged, systemctlOutput)
	if err != nil {
		fmt.Fprintf(os.Stderr, "boxpool up: %v\n", err)
		return 1
	}
	steps, err := upSteps(facts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "boxpool up: %v\n", err)
		return 1
	}
	for _, s := range steps {
		if s[0] == "restart" {
			fmt.Println("restarting boxpool.service")
		}
		if err := systemctl(s...); err != nil {
			fmt.Fprintf(os.Stderr, "boxpool up: %v\n", err)
			return 1
		}
	}

	dir, err := preflight.StateDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "boxpool up: %v\n", err)
		return 1
	}
	fmt.Println("waiting for the daemon; its first start downloads Firecracker and the guest image (about 60 MB)")
	if err := waitHealthy(client.New(dir), upTimeout); err != nil {
		fmt.Fprintf(os.Stderr, "boxpool up: %v; see: journalctl -u boxpool\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	info, err := client.New(dir).Info(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "boxpool up: %v\n", err)
		return 1
	}
	fmt.Printf("boxpool %s is running (backend %s, state in %s) after %v\n", info.Version, info.Backend, info.StateDir, time.Since(start).Round(100*time.Millisecond))
	if info.Warning != "" {
		fmt.Fprintf(os.Stderr, "warning: %s\n", info.Warning)
	}
	return 0
}

// upFacts is the systemd state boxpool up observed after installing its
// files. It decides from these rather than from what this run changed, so
// a run that failed halfway is finished by the next one.
type upFacts struct {
	unitChanged bool   // this run rewrote the unit file
	enabled     string // what systemctl is-enabled printed
	needReload  bool   // systemd has not loaded the unit file on disk
	staleExe    bool   // the service runs, but not the installed binary
}

// systemctlOutput captures stdout separately from errors and stderr, because
// is-enabled reports valid states even when it exits non-zero.
func systemctlOutput(args ...string) ([]byte, error) {
	return exec.Command("systemctl", args...).Output()
}

// observe reads the upFacts from systemd and /proc. It must not plan from
// missing state: a healthy old daemon can otherwise hide a failed upgrade.
func observe(unitChanged bool, output func(...string) ([]byte, error)) (upFacts, error) {
	show := func(prop string) (string, error) {
		out, err := output("show", "-p", prop, "--value", "boxpool")
		if err != nil {
			return "", fmt.Errorf("systemctl show %s: %w", prop, err)
		}
		return strings.TrimSpace(string(out)), nil
	}
	enabled, err := output("is-enabled", "boxpool")
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return upFacts{}, fmt.Errorf("systemctl is-enabled: %w", err)
	}
	state := strings.TrimSpace(string(enabled))
	// Allow future states without a whitelist, but reject empty output and
	// diagnostics in place of a single systemd state word.
	if state == "" || state[0] < 'a' || state[0] > 'z' || strings.Trim(state, "abcdefghijklmnopqrstuvwxyz-") != "" {
		return upFacts{}, fmt.Errorf("systemctl is-enabled: invalid state %q", state)
	}
	reload, err := show("NeedDaemonReload")
	if err != nil {
		return upFacts{}, err
	}
	if reload != "yes" && reload != "no" {
		return upFacts{}, fmt.Errorf("systemctl show NeedDaemonReload: invalid value %q; want yes or no", reload)
	}
	pidText, err := show("MainPID")
	if err != nil {
		return upFacts{}, err
	}
	pid, err := strconv.ParseUint(pidText, 10, 64)
	if err != nil {
		return upFacts{}, fmt.Errorf("systemctl show MainPID: invalid value %q: %w", pidText, err)
	}
	f := upFacts{
		unitChanged: unitChanged,
		enabled:     state,
		needReload:  reload == "yes",
	}
	if pid != 0 {
		// Replacing the binary by rename leaves a running daemon on the
		// old inode; if either stat fails, restarting is the safe answer.
		running, err1 := os.Stat("/proc/" + strconv.FormatUint(pid, 10) + "/exe")
		installed, err2 := os.Stat(installPath)
		f.staleExe = err1 != nil || err2 != nil || !os.SameFile(running, installed)
	}
	return f, nil
}

// upSteps returns the systemctl commands that bring the service up from
// the state in f. enable reloads systemd on every call unless --no-reload,
// and a reload before it would leave its new symlink unseen; so enable
// only when needed and then reload once. A second up with nothing to do
// only runs start, which does nothing if it is running and starts it if it
// failed or was stopped.
func upSteps(f upFacts) ([][]string, error) {
	var steps [][]string
	reload := f.unitChanged || f.needReload
	switch {
	case strings.HasPrefix(f.enabled, "masked"):
		return nil, errors.New("boxpool.service is masked; unmask it first: sudo systemctl unmask boxpool")
	case f.enabled != "enabled": // also enabled-runtime, which a reboot forgets
		steps = append(steps, []string{"enable", "--no-reload", "boxpool"})
		reload = true
	}
	if reload {
		steps = append(steps, []string{"daemon-reload"})
	}
	// A pending reload means the running daemon has the old unit settings.
	if f.unitChanged || f.needReload || f.staleExe {
		return append(steps, []string{"restart", "boxpool"}), nil
	}
	return append(steps, []string{"start", "boxpool"}), nil
}

func down(args []string) int {
	if len(args) != 0 {
		fmt.Fprint(os.Stderr, "Usage: boxpool down\n\nStops and disables the boxpool service. The state directory and the\ninstalled binary are kept. Needs root.\n")
		return 2
	}
	if !systemdOnly("down") {
		return 1
	}
	if err := systemctl("disable", "--now", "boxpool"); err != nil {
		fmt.Fprintf(os.Stderr, "boxpool down: %v\n", err)
		return 1
	}
	fmt.Println("boxpool.service is stopped and disabled; its state is kept, and boxpool up starts it again")
	fmt.Println("running Firecracker VMs are stopped when the daemon next starts (#27 will keep them)")
	return 0
}

func systemctl(args ...string) error {
	cmd := exec.Command("systemctl", args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("systemctl %v: %w", args, err)
	}
	return nil
}

// waitHealthy polls the daemon until it answers, printing progress, or
// fails once timeout has passed or systemd gave up on the service.
func waitHealthy(c *client.Client, timeout time.Duration) error {
	start := time.Now()
	for next := 15 * time.Second; ; time.Sleep(500 * time.Millisecond) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := c.Health(ctx)
		cancel()
		if err == nil {
			return nil
		}
		waited := time.Since(start)
		if waited > timeout {
			return fmt.Errorf("the daemon is not healthy after %v: %w", timeout, err)
		}
		// is-failed succeeds once Restart= has given up.
		if exec.Command("systemctl", "is-failed", "--quiet", "boxpool").Run() == nil {
			return errors.New("boxpool.service failed")
		}
		if waited > next {
			fmt.Printf("  still waiting (%v)\n", waited.Round(time.Second))
			next += 15 * time.Second
		}
	}
}

// installFile atomically replaces path with data and mode, unless it
// already has them, and reports whether it changed anything.
func installFile(path string, data []byte, mode os.FileMode) (bool, error) {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		if fi, err := os.Stat(path); err == nil && fi.Mode() == mode {
			return false, nil
		}
	}
	// In the same directory, so the rename cannot cross file systems.
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return false, err
	}
	defer os.Remove(f.Name()) // fails harmlessly after the rename
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(mode)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		return false, fmt.Errorf("install %s: %w", path, err)
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		d.Sync() // make the rename durable
		d.Close()
	}
	return true, nil
}
