//go:build linux || darwin

// Package diskspace measures capacity available for user-requested VM disks
// and images on the filesystem backing a path.
package diskspace

import (
	"fmt"
	"math"

	"golang.org/x/sys/unix"
)

// Info distinguishes immediately free blocks from capacity macOS expects
// to make available by reclaiming nonessential data. Both are estimates;
// callers must still handle allocation failures.
type Info struct {
	FreeBytes, AvailableBytes int64
}

// Probe queries the path's own filesystem, including when a backend directory
// has been relocated. Linux and non-cgo macOS use immediately free blocks.
func Probe(path string) (Info, error) {
	return probe(path, freeBytes, importantCapacity)
}

func probe(path string, filesystem, important func(string) (int64, error)) (Info, error) {
	free, err := filesystem(path)
	if err != nil {
		return Info{}, err
	}
	info := Info{FreeBytes: free, AvailableBytes: free}
	// A missing, invalid or lagging estimate must not reduce space already
	// known to be free. Native errors retain the conservative statfs result.
	if available, err := important(path); err == nil && available > free {
		info.AvailableBytes = available
	}
	return info, nil
}

func freeBytes(path string) (int64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return blockBytes(stat.Bavail, int64(stat.Bsize))
}

func blockBytes(blocks uint64, size int64) (int64, error) {
	if size <= 0 || blocks > uint64(math.MaxInt64/size) {
		return 0, fmt.Errorf("invalid free disk size")
	}
	return int64(blocks) * size, nil
}
