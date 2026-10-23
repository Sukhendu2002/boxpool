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
