package agent

import (
	"os"
	"syscall"
	"time"
	"unsafe"
)

func setSymlinkMetadata(name string, mode os.FileMode, mtime time.Time) error {
	p, err := syscall.BytePtrFromString(name)
	if err != nil {
		return err
	}
	// Linux symlink permissions are always 0777; utimensat must not follow the link.
	ts := [2]syscall.Timespec{syscall.NsecToTimespec(mtime.UnixNano()), syscall.NsecToTimespec(mtime.UnixNano())}
	const atSymlinkNoFollow = 0x100
	dirfd := -100 // AT_FDCWD permits relative destination paths too.
	_, _, errno := syscall.Syscall6(syscall.SYS_UTIMENSAT, uintptr(dirfd), uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&ts)), atSymlinkNoFollow, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}
