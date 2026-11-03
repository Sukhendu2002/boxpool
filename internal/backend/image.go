package backend

import (
	"fmt"
	"os"
	"path/filepath"
)

// ImageRootfs selects an installed VM image. Desktop artifacts are installed
// separately, keeping base downloads and the warm cache small. Snapshot disks
// already contain their image, so From takes precedence.
func ImageRootfs(base string, s Spec) (string, error) {
	if s.From != "" {
		return s.From, nil
	}
	if s.Image != "desktop" {
		return base, nil
	}
	path := os.Getenv("BOXPOOL_DESKTOP_ROOTFS")
	if path == "" {
		path = filepath.Join(filepath.Dir(base), "rootfs-desktop.ext4")
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("desktop image not installed at %s: build image/ with IMAGE_VARIANT=desktop and set BOXPOOL_DESKTOP_ROOTFS: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("desktop rootfs %s is not a regular file", path)
	}
	return path, nil
}
