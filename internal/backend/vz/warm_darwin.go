//go:build darwin && cgo

package vz

import (
	"context"
	"fmt"
	"path/filepath"

	"boxpool.run/boxpool/internal/backend"
	"golang.org/x/sys/unix"
)

// Warm builds a private paused base without holding user reservations.
// Unsupported save/restore hosts retain the existing cold-boot path.
func (d *Driver) Warm(ctx context.Context) error {
	dir := filepath.Join(filepath.Dir(d.root), "warm", "vz")
	base, err := New(ctx, dir, d.paths)
	if err != nil {
		return err
	}
	defer base.Close()
	osBuild, err := unix.Sysctl("kern.osversion")
	if err != nil {
		return err
	}
	if err := backend.PrepareWarm(ctx, base, dir, fmt.Sprintf("network=%t;os=%s", d.isolated, osBuild), d.paths.Kernel, d.paths.Rootfs); err != nil {
		return err
	}
	l := sandboxLayout(base.root, backend.WarmID)
	d.warm = &l
	return nil
}
