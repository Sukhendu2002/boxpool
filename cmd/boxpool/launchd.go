package main

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"boxpool.run/boxpool/internal/client"
	"boxpool.run/boxpool/internal/preflight"
)

// agentLabel is the launchd label of the daemon's launch agent on macOS.
const agentLabel = "run.boxpool.daemon"

// launchAgent is where boxpool up installs the daemon on macOS, as the
// user: a launch agent in the gui/<uid> domain, which launchd starts at
// every login.
type launchAgent struct {
	label string
	plist string // the plist file
	bin   string // the installed binary
	log   string // the daemon's stdout and stderr
	home  string // BOXPOOL_HOME for the daemon, or "" for the default
}

// userAgent returns the launch agent for this user. A BOXPOOL_HOME set at
// up time is kept in the plist, so the client commands run in the same
// environment find the daemon; the binary lives in that state directory.
func userAgent() (launchAgent, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return launchAgent{}, err
	}
	dir, err := preflight.StateDir()
	if err != nil {
		return launchAgent{}, err
	}
	if dir, err = filepath.Abs(dir); err != nil {
		return launchAgent{}, err
	}
	a := launchAgent{
		label: agentLabel,
		plist: filepath.Join(home, "Library", "LaunchAgents", agentLabel+".plist"),
		bin:   filepath.Join(dir, "bin", "boxpool"),
		log:   filepath.Join(home, "Library", "Logs", "boxpool", "daemon.log"),
	}
	if os.Getenv("BOXPOOL_HOME") != "" {
		a.home = dir
	}
	return a, nil
}

func (a launchAgent) domain() string { return "gui/" + strconv.Itoa(os.Getuid()) }
func (a launchAgent) target() string { return a.domain() + "/" + a.label }

// agentPlist renders the launch agent's plist.
//
// KeepAlive restarts the daemon only when it fails: it exits 0 only on
// SIGTERM or SIGINT, which someone sent on purpose. ExitTimeOut gives its
// shutdown, which stops every VM in stages of up to 10 s, time before
// launchd sends SIGKILL; the default is 5 s. Interactive, because its VMs
// run the user's shells and builds, which Background would throttle. No
// UserName: an agent runs as the user who loads it.
func agentPlist(a launchAgent) []byte {
	var b strings.Builder
	esc := func(s string) string {
		var e strings.Builder
		xml.EscapeText(&e, []byte(s))
		return e.String()
	}
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + esc(a.label) + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + esc(a.bin) + `</string>
		<string>daemon</string>
	</array>
`)
	if a.home != "" {
		b.WriteString(`	<key>EnvironmentVariables</key>
	<dict>
		<key>BOXPOOL_HOME</key>
		<string>` + esc(a.home) + `</string>
	</dict>
`)
	}
	b.WriteString(`	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>ExitTimeOut</key>
	<integer>60</integer>
	<key>ProcessType</key>
	<string>Interactive</string>
	<key>StandardOutPath</key>
	<string>` + esc(a.log) + `</string>
	<key>StandardErrorPath</key>
	<string>` + esc(a.log) + `</string>
</dict>
</plist>
`)
	return []byte(b.String())
}

// agentFacts is the launchd state boxpool up observed before changing it,
// so a run that failed halfway is finished by the next one.
type agentFacts struct {
	disabled   bool // launchctl disable was run for the label
	loaded     bool // launchd has the service
	pid        int  // its running process, or 0
	plistStale bool // the plist on disk is not the one up writes
	staleExe   bool // pid runs, but not the installed binary
}

// agentSteps returns the launchctl commands that bring the agent up from
// the state in f; {"install"} writes the plist. launchd keeps the plist it
// loaded, so a changed plist is booted out before it is replaced: a failed
// bootout then leaves the old file, and the next run sees it stale again.
// With nothing to do it returns no steps, so the daemon keeps its PID.
func agentSteps(f agentFacts, a launchAgent) [][]string {
	var steps [][]string
	if f.disabled { // bootstrap fails on a disabled label
		steps = append(steps, []string{"enable", a.target()})
	}
	if f.loaded && f.plistStale {
		steps = append(steps, []string{"bootout", a.target()})
	}
	if f.plistStale {
		steps = append(steps, []string{"install"})
	}
	if !f.loaded || f.plistStale {
		return append(steps, []string{"bootstrap", a.domain(), a.plist}) // RunAtLoad starts it
	}
	if f.pid == 0 || f.staleExe {
		steps = append(steps, []string{"kickstart", "-k", a.target()})
	}
	return steps
}

// service is the part of launchctl print's output that up uses.
type service struct {
	state    string // running, spawn scheduled, not running, ...
	pid      int
	lastExit string // "(never exited)" until the first exit
}

// parseService reads launchctl print's top-level "key = value" lines.
func parseService(out string) (service, error) {
	var s service
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "\t\t") {
			continue
		}
		key, value, ok := strings.Cut(strings.TrimSpace(line), " = ")
		switch {
		case !ok:
		case key == "state":
			s.state = value
		case key == "last exit code":
			s.lastExit = value
		case key == "pid":
			pid, err := strconv.Atoi(value)
			if err != nil || pid <= 0 {
				return service{}, fmt.Errorf("launchctl print: invalid pid %q", value)
			}
			s.pid = pid
		}
	}
	if s.state == "" {
		return service{}, errors.New("launchctl print: no state")
	}
	return s, nil
}

// parseDisabled reports whether launchctl print-disabled lists label as
// disabled; older macOS prints true for disabled.
func parseDisabled(out, label string) bool {
	for _, line := range strings.Split(out, "\n") {
		name, value, ok := strings.Cut(strings.TrimSpace(line), " => ")
		if ok && name == strconv.Quote(label) {
			return value == "disabled" || value == "true"
		}
	}
	return false
}

// listsPID reports whether lsof -t output lists pid.
func listsPID(out string, pid int) bool {
	for _, line := range strings.Fields(out) {
		if line == strconv.Itoa(pid) {
			return true
		}
	}
	return false
}

// printService runs launchctl print on the agent; ok is false when
// launchd does not have it.
func printService(a launchAgent) (s service, ok bool, err error) {
	out, err := exec.Command("launchctl", "print", a.target()).Output()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 113 { // could not find service
		return service{}, false, nil
	}
	if err != nil {
		return service{}, false, fmt.Errorf("launchctl print %s: %w", a.target(), err)
	}
	s, err = parseService(string(out))
	return s, err == nil, err
}

// observeAgent reads the agentFacts from launchd, lsof and the plist on
// disk, after the binary is installed.
func observeAgent(a launchAgent) (agentFacts, error) {
	var f agentFacts
	out, err := exec.Command("launchctl", "print-disabled", a.domain()).Output()
	if err != nil {
		return agentFacts{}, fmt.Errorf("launchctl print-disabled %s: %w", a.domain(), err)
	}
	f.disabled = parseDisabled(string(out), a.label)
	s, loaded, err := printService(a)
	if err != nil {
		return agentFacts{}, err
	}
	f.loaded, f.pid = loaded, s.pid
	old, err := os.ReadFile(a.plist)
	f.plistStale = err != nil || string(old) != string(agentPlist(a))
	if f.pid != 0 {
		// Replacing the binary by rename leaves a running daemon on the
		// old inode, which lsof -t <file> does not list: it matches the
		// file's current inode. It also exits 1 for no match, and on any
		// failure restarting is the safe answer.
		out, _ := exec.Command("lsof", "-t", a.bin).Output()
		f.staleExe = !listsPID(string(out), f.pid)
	}
	return f, nil
}

// hasVirtualization reports whether codesign's XML entitlements grant
// com.apple.security.virtualization.
func hasVirtualization(entitlements []byte) bool {
	return strings.Contains(strings.Join(strings.Fields(string(entitlements)), ""), "<key>com.apple.security.virtualization</key><true/>")
}

// installAgent installs bin (the bytes of the binary at exe) and the plist
// for a, then starts or restarts the agent as its observed state needs.
func installAgent(a launchAgent, exe string, bin []byte) error {
	ent, err := exec.Command("codesign", "-d", "--entitlements", "-", "--xml", exe).Output()
	if err != nil || !hasVirtualization(ent) {
		return fmt.Errorf("%s is not signed with the com.apple.security.virtualization entitlement, which the daemon needs to run VMs; sign it first: scripts/dev-sign.sh %s", exe, exe)
	}
	for _, dir := range []string{filepath.Dir(a.bin), filepath.Dir(a.log), filepath.Dir(a.plist)} {
		if err := os.MkdirAll(dir, 0750); err != nil {
			return err
		}
	}
	changed, err := installFile(a.bin, bin, 0755)
	if err != nil {
		return err
	}
	report(changed, a.bin)
	// A copy keeps the embedded signature; check it survived.
	if out, err := exec.Command("codesign", "-v", a.bin).CombinedOutput(); err != nil {
		return fmt.Errorf("codesign -v %s: %v: %s", a.bin, err, strings.TrimSpace(string(out)))
	}
	f, err := observeAgent(a)
	if err != nil {
		return err
	}
	steps := agentSteps(f, a)
	if !f.plistStale {
		report(false, a.plist)
	}
	for _, s := range steps {
		switch s[0] {
		case "install":
			if _, err := installFile(a.plist, agentPlist(a), 0644); err != nil {
				return err
			}
			report(true, a.plist)
			continue
		case "kickstart":
			fmt.Println("restarting " + a.label)
		}
		if err := launchctl(s...); err != nil {
			return err
		}
	}
	return nil
}

func report(changed bool, path string) {
	if changed {
		fmt.Printf("installed %s\n", path)
	} else {
		fmt.Printf("%s is up to date\n", path)
	}
}

// agentFailed returns an error once launchd has given up on the agent or
// the daemon has exited since it was started.
func agentFailed(a launchAgent) error {
	s, ok, err := printService(a)
	switch {
	case err != nil:
		return err
	case !ok:
		return fmt.Errorf("launchd does not have %s", a.label)
	case s.state != "running" && s.lastExit != "" && s.lastExit != "(never exited)":
		return fmt.Errorf("the daemon exited (last exit code %s)", s.lastExit)
	}
	return nil
}

// stopAgent boots the agent out, which stops the daemon and its VMs, and
// removes its plist so that the next login does not start it again.
// Removing it rather than launchctl disable leaves no override for up to
// undo: up writes the plist again.
func stopAgent(a launchAgent) error {
	if _, loaded, err := printService(a); err != nil {
		return err
	} else if loaded {
		if err := launchctl("bootout", a.target()); err != nil {
			return err
		}
	}
	if err := os.Remove(a.plist); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func launchctl(args ...string) error {
	cmd := exec.Command("launchctl", args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("launchctl %v: %w", args, err)
	}
	return nil
}

// launchdOnly returns false after printing why cmd cannot run here.
func launchdOnly(cmd string) bool {
	if os.Geteuid() == 0 {
		fmt.Fprintf(os.Stderr, "boxpool %s: run it as yourself, not root: on macOS the daemon runs as your user\n", cmd)
		return false
	}
	return true
}

func upLaunchd(args []string) int {
	if len(args) != 0 {
		fmt.Fprint(os.Stderr, "Usage: boxpool up\n\nInstalls this binary in the state directory and runs it as the launch\nagent "+agentLabel+", then waits until the daemon is healthy. The\nbinary must be signed with the virtualization entitlement\n(scripts/dev-sign.sh). Run it as yourself, not root.\n")
		return 2
	}
	if !launchdOnly("up") {
		return 1
	}
	if r := preflight.Run(); !r.Ready() {
		printReport(r)
		fmt.Fprintln(os.Stderr, "\nboxpool up: this machine cannot host sandboxes yet; fix the checks marked ✗ above")
		return 1
	}
	start := time.Now()
	a, err := userAgent()
	if err != nil {
		fmt.Fprintf(os.Stderr, "boxpool up: %v\n", err)
		return 1
	}
	exe, bin, err := self()
	if err != nil {
		fmt.Fprintf(os.Stderr, "boxpool up: read this binary: %v\n", err)
		return 1
	}
	if err := installAgent(a, exe, bin); err != nil {
		fmt.Fprintf(os.Stderr, "boxpool up: %v\n", err)
		return 1
	}
	dir := filepath.Dir(filepath.Dir(a.bin))
	fmt.Println("waiting for the daemon; its first start downloads the guest image (about 45 MB)")
	if err := waitHealthy(client.New(dir), upTimeout, func() error { return agentFailed(a) }); err != nil {
		fmt.Fprintf(os.Stderr, "boxpool up: %v; see %s\n", err, a.log)
		return 1
	}
	return printRunning(dir, start)
}

func downLaunchd(args []string) int {
	if len(args) != 0 {
		fmt.Fprint(os.Stderr, "Usage: boxpool down\n\nStops the launch agent "+agentLabel+" and its sandboxes and removes its\nplist, so it does not start at the next login. The state directory and\nthe installed binary are kept. Run it as yourself, not root.\n")
		return 2
	}
	if !launchdOnly("down") {
		return 1
	}
	a, err := userAgent()
	if err == nil {
		err = stopAgent(a)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "boxpool down: %v\n", err)
		return 1
	}
	fmt.Printf("%s is stopped and removed with its sandboxes' VMs; its state in %s is kept, and boxpool up starts it again\n", a.label, filepath.Dir(filepath.Dir(a.bin)))
	return 0
}

// self returns the path and bytes of the running binary.
func self() (string, []byte, error) {
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	var bin []byte
	if err == nil {
		bin, err = os.ReadFile(exe)
	}
	return exe, bin, err
}

// printRunning prints the healthy daemon's info.
func printRunning(dir string, start time.Time) int {
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
