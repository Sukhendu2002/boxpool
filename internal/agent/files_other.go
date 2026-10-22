//go:build !linux && !darwin

package agent

import (
	"errors"
	"os"
	"time"
)

func setSymlinkMetadata(name string, mode os.FileMode, mtime time.Time) error {
	return errors.New("symlink metadata is only supported on Linux and macOS")
}
