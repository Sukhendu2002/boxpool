//go:build linux || darwin

package preflight

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

const minDiskBytes uint64 = 20_000_000_000

func diskCheck() Check {
	path, err := StateDir()
	if err != nil {
		return Check{Name: "free disk", Required: true, Detail: "could not find home directory: " + err.Error()}
	}
	return diskCheckAt(path)
}

func diskCheckAt(path string) Check {
	c := Check{Name: "free disk", Required: true}
	path, err := filepath.Abs(path)
	if err != nil {
		c.Detail = "could not resolve state directory: " + err.Error()
		return c
	}
	for {
		info, err := os.Stat(path)
		if err == nil {
			if !info.IsDir() {
				c.Detail = path + " is not a directory"
				return c
			}
			break
		}
		if !os.IsNotExist(err) || filepath.Dir(path) == path {
			c.Detail = "could not check free disk at " + path + ": " + err.Error()
			return c
		}
		path = filepath.Dir(path)
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		c.Detail = "could not check free disk at " + path + ": " + err.Error()
		return c
	}
	return diskSpaceCheck(uint64(stat.Bavail)*uint64(stat.Bsize), path)
}

func diskSpaceCheck(freeBytes uint64, path string) Check {
	return Check{
		Name:     "free disk",
		OK:       freeBytes >= minDiskBytes,
		Required: true,
		Detail:   fmt.Sprintf("%d GB free at %s (need 20 GB)", freeBytes/1_000_000_000, path),
	}
}
