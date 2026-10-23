//go:build linux

package firecracker

import (
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// cloneFile makes dst share src's blocks; tests replace it to force the copy.
var cloneFile = unix.IoctlFileClone

// copyRootfs creates dst as a copy of src: a reflink where the file system
// supports it (btrfs, XFS), otherwise a copy that keeps src's holes.
func copyRootfs(dst, src string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(dst)
		}
	}()
	err = cloneFile(int(out.Fd()), int(in.Fd()))
	if errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EXDEV) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOTTY) {
		err = copySparse(out, in)
	}
	return err
}

// copySparse copies only in's data regions, so holes stay holes.
func copySparse(out, in *os.File) error {
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	for off := int64(0); off < fi.Size(); {
		data, err := in.Seek(off, unix.SEEK_DATA)
		if errors.Is(err, unix.ENXIO) {
			break // only a hole is left
		}
		if err != nil {
			return err
		}
		hole, err := in.Seek(data, unix.SEEK_HOLE)
		if err != nil {
			return err
		}
		if _, err := in.Seek(data, io.SeekStart); err != nil {
			return err
		}
		if _, err := out.Seek(data, io.SeekStart); err != nil {
			return err
		}
		// File to file, io.CopyN lets the kernel copy (copy_file_range).
		if _, err := io.CopyN(out, in, hole-data); err != nil {
			return err
		}
		off = hole
	}
	return out.Truncate(fi.Size())
}
