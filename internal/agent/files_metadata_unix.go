//go:build linux || darwin

package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"time"

	"golang.org/x/sys/unix"
)

// Pin the symlink's parent through Root. Neither a renamed destination nor
// swapped parent symlinks may redirect these no-follow metadata operations.
func setRootSymlinkMetadata(root *os.Root, name string, mode os.FileMode, mtime time.Time) error {
	parent, err := root.Open(filepath.Dir(name))
	if err != nil {
		return err
	}
	defer parent.Close()
	base := filepath.Base(name)
	if runtime.GOOS == "darwin" {
		if err := unix.Fchmodat(int(parent.Fd()), base, uint32(mode.Perm()), unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
	}
	ts := []unix.Timespec{unix.NsecToTimespec(mtime.UnixNano()), unix.NsecToTimespec(mtime.UnixNano())}
	return unix.UtimesNanoAt(int(parent.Fd()), base, ts, unix.AT_SYMLINK_NOFOLLOW)
}
