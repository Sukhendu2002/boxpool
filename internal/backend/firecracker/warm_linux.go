//go:build linux

package firecracker

import (
	"context"
	"fmt"
	"path/filepath"

	"boxpool.run/boxpool/internal/backend"
)

// Warm prepares the default-size base before the daemon serves requests.
// Its separate state directory keeps the paused cache out of user admission.
func (d *Driver) Warm(ctx context.Context) error {
	dir := filepath.Join(filepath.Dir(d.root), "warm", "firecracker")
	// Do not sweep the shared network store against the cache's sandbox
	// directory: the user's running VMs live under d.root, not this root.
	base, err := New(ctx, dir, d.paths, nil)
	if err != nil {
		return err
	}
	base.store, base.networkRoot = d.store, d.root
	l := sandboxLayout(base.root, backend.WarmID)
	freeze := func(ctx context.Context) error {
		ag := base.agent(l)
		defer ag.CloseIdleConnections()
		return backend.FreezeGuest(ctx, ag, true)
	}
	if err := backend.PrepareWarm(ctx, base, dir, fmt.Sprintf("network=%t", d.store != nil), freeze, d.paths.Firecracker, d.paths.Kernel, d.paths.Rootfs); err != nil {
		return err
	}
	d.warm = &l
	return nil
}
