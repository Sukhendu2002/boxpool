//go:build linux || darwin

package preflight

import (
	"fmt"
	"os"
	"path/filepath"

	"boxpool.run/boxpool/internal/diskspace"
)

const minDiskBytes int64 = 20_000_000_000

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
	space, err := diskspace.Probe(path)
	if err != nil {
		c.Detail = "could not check free disk at " + path + ": " + err.Error()
		return c
	}
	return diskSpaceCheck(space, path)
}

func diskSpaceCheck(space diskspace.Info, path string) Check {
	detail := fmt.Sprintf("%d GB free at %s (need 20 GB)", space.FreeBytes/1_000_000_000, path)
	if space.AvailableBytes > space.FreeBytes {
		detail = fmt.Sprintf("%d GB available at %s (%d GB immediately free; includes reclaimable space; need 20 GB)",
			space.AvailableBytes/1_000_000_000, path, space.FreeBytes/1_000_000_000)
	}
	return Check{
		Name:     "free disk",
		OK:       space.AvailableBytes >= minDiskBytes,
		Required: true,
		Detail:   detail,
	}
}
