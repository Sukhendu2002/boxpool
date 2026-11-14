package resources

import (
	"fmt"
	"runtime"

	"boxpool.run/boxpool/internal/diskspace"

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
	space, err := diskspace.Probe(path)
	if err != nil {
		return c, err
	}
	c.DiskBytes = space.AvailableBytes
	return c, nil
}
