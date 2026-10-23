//go:build linux || darwin

package main

import (
	"os"
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
	if os.Geteuid() == 0 && runtime.GOOS == "linux" {
		t.Skip("as root on Linux, up would install the service")
	}
	want := "must run as root"
	if runtime.GOOS == "darwin" {
		want = "not supported on macOS yet (#22)"
	}
	for _, cmd := range []string{"up", "down"} {
		if code, _, errOut := cli(t, t.TempDir(), "", cmd); code != 1 || !strings.Contains(errOut, want) {
			t.Errorf("boxpool %s = %d %q, want %q", cmd, code, errOut, want)
		}
	}
}
