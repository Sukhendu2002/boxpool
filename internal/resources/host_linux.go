//go:build linux

package resources

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func hostCapacity(path string) (Capacity, error) {
	var c Capacity
	var set unix.CPUSet
	if err := unix.SchedGetaffinity(0, &set); err != nil {
		return c, err
	}
	for i := 0; i < 1024; i++ {
		if set.IsSet(i) {
			c.CPUs = append(c.CPUs, i)
		}
	}
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return c, err
	}
	c.MemoryMB, c.AvailableMB, err = memoryInfo(string(b))
	if err != nil {
		return c, err
	}
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return c, err
	}
	if stat.Bsize <= 0 || stat.Bavail > uint64((1<<63-1)/stat.Bsize) {
		return c, fmt.Errorf("invalid free disk size")
	}
	c.DiskBytes = int64(stat.Bavail) * stat.Bsize
	return c, nil
}

func memoryInfo(text string) (total, available int64, err error) {
	values := map[string]int64{}
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || (f[0] != "MemTotal:" && f[0] != "MemAvailable:") {
			continue
		}
		if len(f) != 3 || f[2] != "kB" {
			return 0, 0, fmt.Errorf("invalid /proc/meminfo line %q", line)
		}
		n, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil || n < 0 {
			return 0, 0, fmt.Errorf("invalid /proc/meminfo line %q", line)
		}
		values[f[0]] = n / 1024
	}
	total, totalOK := values["MemTotal:"]
	available, availableOK := values["MemAvailable:"]
	if !totalOK || !availableOK || total < 1 || available > total {
		return 0, 0, fmt.Errorf("missing or invalid MemTotal/MemAvailable in /proc/meminfo")
	}
	return total, available, nil
}
