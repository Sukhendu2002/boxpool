package backend

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"boxpool.run/boxpool/internal/artifacts"
)

// ImageRootfs selects a VM image, fetching the pinned desktop disk only when
// requested. Snapshot disks already contain their image, so From takes precedence.
func ImageRootfs(ctx context.Context, base string, s Spec) (string, error) {
	return imageRootfs(ctx, base, s, artifacts.DesktopRootfs)
}

func imageRootfs(ctx context.Context, base string, s Spec, desktop func(context.Context, string) (string, error)) (string, error) {
	if s.From != "" {
		return s.From, nil
	}
	if s.Image != "desktop" {
		return base, nil
	}
	path := os.Getenv("BOXPOOL_DESKTOP_ROOTFS")
	if path == "" {
		return desktop(ctx, filepath.Dir(base))
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("desktop image override %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("desktop rootfs %s is not a regular file", path)
	}
	return path, nil
}
