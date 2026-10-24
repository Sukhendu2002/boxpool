//go:build linux || darwin

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestUnit(t *testing.T) {
	golden(t, "boxpool.service", unit(installPath))
}

func TestInstallFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boxpool")
	for _, c := range []struct {
		data    string
		mode    os.FileMode
		changed bool
	}{
		{"v1", 0755, true},
		{"v1", 0755, false}, // nothing to do
		{"v2", 0755, true},
		{"v2", 0644, true}, // same bytes, other mode
	} {
		changed, err := installFile(path, []byte(c.data), c.mode)
		if err != nil || changed != c.changed {
			t.Fatalf("installFile(%q, %v) = %v, %v; want changed %v", c.data, c.mode, changed, err, c.changed)
		}
		got, _ := os.ReadFile(path)
		fi, _ := os.Stat(path)
		if string(got) != c.data || fi.Mode() != c.mode {
			t.Errorf("after installFile(%q, %v): %q %v", c.data, c.mode, got, fi.Mode())
		}
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Errorf("temporary files left: %v", entries)
	}
	if _, err := installFile(filepath.Join(t.TempDir(), "missing", "boxpool"), nil, 0755); err == nil {
		t.Error("installFile into a missing directory succeeded")
	}
}

func TestUpSteps(t *testing.T) {
	const (
		enable  = "enable --no-reload boxpool; "
		reload  = "daemon-reload; "
		restart = "restart boxpool"
		start   = "start boxpool"
	)
	for _, c := range []struct {
		name string
		f    upFacts
		want string
	}{
		{"first install", upFacts{unitChanged: true, enabled: "disabled", needReload: true}, enable + reload + restart},
		{"unit file changed", upFacts{unitChanged: true, enabled: "enabled", needReload: true}, reload + restart},
		{"nothing to do", upFacts{enabled: "enabled"}, start}, // no reload, no restart
		{"failed or stopped", upFacts{enabled: "enabled"}, start},
		{"after down", upFacts{enabled: "disabled"}, enable + reload + start},
		{"not found", upFacts{enabled: ""}, enable + reload + start},
		// An earlier daemon-reload failed after the unit file was written.
		{"reload pending", upFacts{enabled: "enabled", needReload: true}, reload + restart},
		// Enabled until the next reboot only.
		{"enabled-runtime", upFacts{enabled: "enabled-runtime"}, enable + reload + start},
		// An earlier restart failed after the binary was replaced.
		{"old binary running", upFacts{enabled: "enabled", staleExe: true}, restart},
	} {
		steps, err := upSteps(c.f)
		var got []string
		for _, s := range steps {
			got = append(got, strings.Join(s, " "))
		}
		if g := strings.Join(got, "; "); err != nil || g != c.want {
			t.Errorf("%s: upSteps(%+v) = %q, %v; want %q", c.name, c.f, g, err, c.want)
		}
	}
	for _, state := range []string{"masked", "masked-runtime"} {
		if steps, err := upSteps(upFacts{enabled: state}); err == nil || !strings.Contains(err.Error(), "systemctl unmask boxpool") {
			t.Errorf("upSteps(%s) = %v, %v; want an error that says to unmask", state, steps, err)
		}
	}
}

func TestUpDownRefused(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("as a user on macOS, up and down manage the real launch agent")
	}
	if os.Geteuid() == 0 {
		t.Skip("as root on Linux, up would install the service")
	}
	want := "must run as root"
	for _, cmd := range []string{"up", "down"} {
		if code, _, errOut := cli(t, t.TempDir(), "", cmd); code != 1 || !strings.Contains(errOut, want) {
			t.Errorf("boxpool %s = %d %q, want %q", cmd, code, errOut, want)
		}
	}
}

func TestObserve(t *testing.T) {
	queryErr := errors.New("systemd query failed")
	launchErr := &exec.Error{Name: "systemctl", Err: exec.ErrNotFound}
	exitErr := &exec.ExitError{Stderr: []byte("unit is not enabled")}
	for _, c := range []struct {
		name        string
		enabled     string
		reload      string
		pid         string
		failCommand string
		commandErr  error
		unitChanged bool
		want        upFacts
		wantErr     string
	}{
		{name: "enabled no reload PID zero", enabled: "enabled\n", reload: "no\n", pid: "0\n", want: upFacts{enabled: "enabled"}},
		{name: "reload pending unit changed", enabled: "enabled", reload: "yes", pid: "0", unitChanged: true, want: upFacts{unitChanged: true, enabled: "enabled", needReload: true}},
		// A binary-only upgrade must not turn a failed PID query into start:
		// the old daemon could still answer the subsequent health check.
		{name: "MainPID query fails during binary upgrade", enabled: "enabled", reload: "no", failCommand: "show -p MainPID --value boxpool", commandErr: queryErr, wantErr: "systemctl show MainPID"},
		{name: "MainPID query fails with output", enabled: "enabled", reload: "no", pid: "0", failCommand: "show -p MainPID --value boxpool", commandErr: queryErr, wantErr: "systemctl show MainPID"},
		{name: "reload query fails", enabled: "enabled", failCommand: "show -p NeedDaemonReload --value boxpool", commandErr: queryErr, wantErr: "systemctl show NeedDaemonReload"},
		{name: "reload query fails with output", enabled: "enabled", reload: "no", failCommand: "show -p NeedDaemonReload --value boxpool", commandErr: queryErr, wantErr: "systemctl show NeedDaemonReload"},
		{name: "reload empty", enabled: "enabled", pid: "0", wantErr: "NeedDaemonReload: invalid value"},
		{name: "reload malformed", enabled: "enabled", reload: "maybe", pid: "0", wantErr: "NeedDaemonReload: invalid value"},
		{name: "reload uppercase", enabled: "enabled", reload: "YES", pid: "0", wantErr: "NeedDaemonReload: invalid value"},
		{name: "PID empty", enabled: "enabled", reload: "no", wantErr: "MainPID: invalid value"},
		{name: "PID malformed", enabled: "enabled", reload: "no", pid: "abc", wantErr: "MainPID: invalid value"},
		{name: "PID negative", enabled: "enabled", reload: "no", pid: "-1", wantErr: "MainPID: invalid value"},
		{name: "PID signed", enabled: "enabled", reload: "no", pid: "+1", wantErr: "MainPID: invalid value"},
		{name: "PID nondecimal", enabled: "enabled", reload: "no", pid: "0x10", wantErr: "MainPID: invalid value"},
		{name: "PID multiple values", enabled: "enabled", reload: "no", pid: "1\n2", wantErr: "MainPID: invalid value"},
		{name: "PID overflow", enabled: "enabled", reload: "no", pid: "18446744073709551616", wantErr: "MainPID: invalid value"},
		{name: "disabled nonzero exit", enabled: "disabled\n", reload: "no", pid: "0", failCommand: "is-enabled boxpool", commandErr: exitErr, want: upFacts{enabled: "disabled"}},
		{name: "masked nonzero exit", enabled: "masked\n", reload: "no", pid: "0", failCommand: "is-enabled boxpool", commandErr: exitErr, want: upFacts{enabled: "masked"}},
		{name: "runtime enabled", enabled: "enabled-runtime", reload: "no", pid: "0", want: upFacts{enabled: "enabled-runtime"}},
		{name: "other state", enabled: "linked-runtime", reload: "no", pid: "0", want: upFacts{enabled: "linked-runtime"}},
		{name: "future state nonzero exit", enabled: "future-state", reload: "no", pid: "0", failCommand: "is-enabled boxpool", commandErr: exitErr, want: upFacts{enabled: "future-state"}},
		{name: "is-enabled launch error", failCommand: "is-enabled boxpool", commandErr: launchErr, wantErr: "systemctl is-enabled"},
		{name: "is-enabled other error with output", enabled: "enabled", failCommand: "is-enabled boxpool", commandErr: queryErr, wantErr: "systemctl is-enabled"},
		{name: "is-enabled empty", wantErr: "is-enabled: invalid state"},
		{name: "is-enabled empty nonzero exit", failCommand: "is-enabled boxpool", commandErr: exitErr, wantErr: "is-enabled: invalid state"},
		{name: "is-enabled diagnostic nonzero exit", enabled: "Failed to connect to bus", failCommand: "is-enabled boxpool", commandErr: exitErr, wantErr: "is-enabled: invalid state"},
		{name: "nonzero PID stat failure", enabled: "enabled", reload: "no", pid: "999999999", want: upFacts{enabled: "enabled", staleExe: true}},
	} {
		t.Run(c.name, func(t *testing.T) {
			output := func(args ...string) ([]byte, error) {
				command := strings.Join(args, " ")
				var out string
				switch command {
				case "is-enabled boxpool":
					out = c.enabled
				case "show -p NeedDaemonReload --value boxpool":
					out = c.reload
				case "show -p MainPID --value boxpool":
					out = c.pid
				default:
					t.Fatalf("unexpected command: systemctl %s", command)
				}
				if command == c.failCommand {
					return []byte(out), c.commandErr
				}
				return []byte(out), nil
			}
			got, err := observe(c.unitChanged, output)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("observe = %+v, %v; want error containing %q", got, err, c.wantErr)
				}
				if got != (upFacts{}) {
					t.Errorf("failed observation returned partial facts: %+v", got)
				}
				var exitErr *exec.ExitError
				if c.commandErr != nil && !errors.As(c.commandErr, &exitErr) && !errors.Is(err, c.commandErr) {
					t.Errorf("observation did not preserve command error: %v", err)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("observe = %+v, %v; want %+v", got, err, c.want)
			}
		})
	}
}
