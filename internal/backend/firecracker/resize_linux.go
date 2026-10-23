//go:build linux

package firecracker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// resizeFilesystem is replaced by driver unit tests with a tiny fake image.
var resizeFilesystem = func(ctx context.Context, path string) error {
	out, err := exec.CommandContext(ctx, "resize2fs", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("resize2fs (install e2fsprogs): %w: %s", err, out)
	}
	return nil
}

// growRootfs never shrinks an image or changes the cached base. Truncate
// grows the private block device sparsely; resize2fs grows the unmounted
// ext4 filesystem to fill it without needing root or a loop device.
func growRootfs(ctx context.Context, path string, size int64) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if size < fi.Size() {
		return fmt.Errorf("requested disk is smaller than base image (%d bytes)", fi.Size())
	}
	if size == fi.Size() {
		return nil
	}
	if err := os.Truncate(path, size); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	return resizeFilesystem(ctx, path)
}
