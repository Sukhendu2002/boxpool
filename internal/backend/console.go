package backend

import (
	"errors"
	"io"
	"io/fs"
	"os"
)

// ConsoleMax is the size at which RotateConsole rotates a console log.
const ConsoleMax = 1 << 20

// RotateConsole copies path to path.1, replacing it, and empties path once
// path is larger than ConsoleMax. The VM writes path with O_APPEND, so it
// carries on at the new end.
func RotateConsole(path string) error {
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) || err == nil && fi.Size() <= ConsoleMax {
		return nil
	}
	if err != nil {
		return err
	}
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(path+".1", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	_, err = io.Copy(dst, src)
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Truncate(path, 0)
}
