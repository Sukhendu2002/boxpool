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
	// setattrlist with FSOPT_NOFOLLOW changes the link, including dangling links.
	attrs := struct {
		Count, Reserved                 uint16
		Common, Volume, Dir, File, Fork uint32
	}{Count: 5, Common: 0x400 | 0x20000} // ATTR_CMN_MODTIME | ATTR_CMN_ACCESSMASK
	values := struct {
		Time syscall.Timespec
		Mode uint32
	}{Time: syscall.NsecToTimespec(mtime.UnixNano()), Mode: uint32(mode.Perm())}
	_, _, errno := syscall.Syscall6(syscall.SYS_SETATTRLIST, uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&attrs)), uintptr(unsafe.Pointer(&values)), unsafe.Sizeof(values), 1, 0)
	if errno != 0 {
		return errno
	}
	return nil
}
