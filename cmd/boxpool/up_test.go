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
	for _, c := range []struct {
		bin, unit, enabled bool
		want               string
	}{
		{true, true, false, "enable --no-reload boxpool; daemon-reload; restart boxpool"}, // first run
		{false, true, true, "enable --no-reload boxpool; daemon-reload; restart boxpool"},
		{true, false, true, "restart boxpool"},
		{false, false, false, "enable --no-reload boxpool; daemon-reload; start boxpool"}, // after down
		{false, false, true, "start boxpool"},                                             // nothing changed: no reload, no restart
	} {
		var got []string
		for _, s := range upSteps(c.bin, c.unit, c.enabled) {
			got = append(got, strings.Join(s, " "))
		}
		if g := strings.Join(got, "; "); g != c.want {
			t.Errorf("upSteps(%v, %v, %v) = %q, want %q", c.bin, c.unit, c.enabled, g, c.want)
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
