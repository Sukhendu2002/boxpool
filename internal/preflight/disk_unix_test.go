//go:build linux || darwin

package preflight

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"boxpool.run/boxpool/internal/diskspace"
)

func TestDiskSpaceCheck(t *testing.T) {
	cases := []struct {
		free int64
		want bool
	}{
		{0, false},
		{minDiskBytes - 1, false},
		{minDiskBytes, true},
		{41_000_000_000, true},
	}
	for _, c := range cases {
		t.Run(fmt.Sprint(c.free), func(t *testing.T) {
			got := diskSpaceCheck(diskspace.Info{FreeBytes: c.free, AvailableBytes: c.free}, "/state")
			if got.OK != c.want || !got.Required || got.Warn {
				t.Errorf("diskSpaceCheck(%d) = %+v", c.free, got)
			}
			wantDetail := fmt.Sprintf("%d GB free at /state (need 20 GB)", c.free/1_000_000_000)
			if got.Detail != wantDetail {
				t.Errorf("Detail = %q, want %q", got.Detail, wantDetail)
			}
		})
	}
}

func TestDiskSpaceCheckReclaimable(t *testing.T) {
	for _, tc := range []struct {
		available int64
		want      bool
	}{
		{19_999_999_999, false},
		{20_000_000_000, true},
		{53_000_000_000, true},
	} {
		got := diskSpaceCheck(diskspace.Info{FreeBytes: 16_000_000_000, AvailableBytes: tc.available}, "/state")
		want := fmt.Sprintf("%d GB available at /state (16 GB immediately free; includes reclaimable space; need 20 GB)", tc.available/1_000_000_000)
		if got.OK != tc.want || !got.Required || got.Warn || got.Detail != want {
			t.Errorf("available %d: %+v", tc.available, got)
		}
	}
}

func diskDetailAt(c Check, path string) bool {
	return strings.Contains(c.Detail, " at "+path+" (") && strings.Contains(c.Detail, "need 20 GB)")
}

func TestDiskCheckAt(t *testing.T) {
	dir := t.TempDir()
	for _, path := range []string{dir, filepath.Join(dir, "missing", "state")} {
		c := diskCheckAt(path)
		if !diskDetailAt(c, dir) || !c.Required {
			t.Errorf("diskCheckAt(%q) = %+v", path, c)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "missing")); !os.IsNotExist(err) {
		t.Errorf("disk check should not create directories: %v", err)
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	c := diskCheckAt(file)
	if c.OK || !c.Required || c.Detail != file+" is not a directory" {
		t.Errorf("diskCheckAt(file) = %+v", c)
	}
}

func TestDiskCheckStateDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BOXPOOL_HOME", filepath.Join(dir, "state"))
	c := diskCheck()
	if !diskDetailAt(c, dir) {
		t.Errorf("BOXPOOL_HOME ignored: %+v", c)
	}

	t.Setenv("BOXPOOL_HOME", "")
	if runtime.GOOS == "darwin" {
		t.Setenv("HOME", dir)
		path := filepath.Join(dir, "Library", "Application Support", "boxpool")
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		c = diskCheck()
		if !diskDetailAt(c, path) {
			t.Errorf("macOS default state directory ignored: %+v", c)
		}
	} else {
		if got, want := diskCheck(), diskCheckAt("/var/lib/boxpool"); got != want {
			t.Errorf("Linux default state directory: %+v, want %+v", got, want)
		}
	}
}
