//go:build linux || darwin

package preflight

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDiskSpaceCheck(t *testing.T) {
	cases := []struct {
		free uint64
		want bool
	}{
		{0, false},
		{minDiskBytes - 1, false},
		{minDiskBytes, true},
		{41_000_000_000, true},
	}
	for _, c := range cases {
		t.Run(fmt.Sprint(c.free), func(t *testing.T) {
			got := diskSpaceCheck(c.free, "/state")
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

func TestDiskCheckAt(t *testing.T) {
	dir := t.TempDir()
	for _, path := range []string{dir, filepath.Join(dir, "missing", "state")} {
		c := diskCheckAt(path)
		if !strings.Contains(c.Detail, " GB free at "+dir+" (need 20 GB)") || !c.Required {
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
	if !strings.Contains(c.Detail, " GB free at "+dir+" (need 20 GB)") {
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
		if !strings.Contains(c.Detail, " GB free at "+path+" (need 20 GB)") {
			t.Errorf("macOS default state directory ignored: %+v", c)
		}
	} else {
		if got, want := diskCheck(), diskCheckAt("/var/lib/boxpool"); got != want {
			t.Errorf("Linux default state directory: %+v, want %+v", got, want)
		}
	}
}
