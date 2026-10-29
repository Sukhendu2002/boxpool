//go:build darwin && cgo

package vz

import (
	"context"
	"errors"
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
	freeze := func(ctx context.Context) error {
		m := base.running(backend.WarmID)
		if m == nil {
			return errors.New("warm base stopped before filesystem freeze")
		}
		if m.saveRestoreErr != nil {
			return m.saveRestoreErr
		}
		ag := m.agent()
		defer ag.CloseIdleConnections()
		return backend.FreezeGuest(ctx, ag, true)
	}
	if err := backend.PrepareWarm(ctx, base, dir, fmt.Sprintf("network=%t;os=%s", d.isolated, osBuild), freeze, d.paths.Kernel, d.paths.Rootfs); err != nil {
		return err
	}
	l := sandboxLayout(base.root, backend.WarmID)
	d.warm = &l
	return nil
}
