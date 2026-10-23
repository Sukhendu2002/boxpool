package resources

import (
	"fmt"
	"runtime"

	"golang.org/x/sys/unix"
)

func hostCapacity(path string) (Capacity, error) {
	var c Capacity
	for i := range runtime.NumCPU() {
		c.CPUs = append(c.CPUs, i)
	}
	total, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return c, err
	}
	// The kernel's estimate of free memory, as memory_pressure prints it:
	// unlike free pages it counts what macOS can reclaim or compress.
	level, err := unix.SysctlUint32("kern.memorystatus_level")
	if err != nil {
		return c, err
	}
	if total < 1<<20 || level > 100 {
		return c, fmt.Errorf("invalid hw.memsize %d or kern.memorystatus_level %d", total, level)
	}
	c.MemoryMB = int64(total >> 20)
	c.AvailableMB = c.MemoryMB * int64(level) / 100
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return c, err
	}
	if stat.Bsize == 0 || stat.Bavail > uint64((1<<63-1)/int64(stat.Bsize)) {
		return c, fmt.Errorf("invalid free disk size")
	}
	c.DiskBytes = int64(stat.Bavail) * int64(stat.Bsize)
	return c, nil
}
