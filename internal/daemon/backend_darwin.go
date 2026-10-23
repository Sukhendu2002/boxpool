//go:build darwin && cgo

package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"boxpool.run/boxpool/internal/artifacts"
	"boxpool.run/boxpool/internal/backend/vz"
	"boxpool.run/boxpool/internal/store"
)

func openDriver(ctx context.Context, dir string, info *Info, st *store.Store) (Driver, error) {
	if info.Backend != "virtualization framework" {
		return nil, nil
	}
	if info.Arch != "arm64" {
		return nil, fmt.Errorf("the Virtualization framework backend requires Apple silicon")
	}
	slog.Info("fetching the guest image; the first time downloads about 45 MB")
	start := time.Now()
	paths, err := artifacts.Fetch(ctx, filepath.Join(dir, artifacts.DirName))
	if err != nil {
		return nil, fmt.Errorf("fetch artifacts: %w", err)
	}
	slog.Info("artifacts ready", "took", time.Since(start).Round(time.Millisecond))
	info.Warning = "sandboxes have no network: macOS networking is not implemented yet (#21)"
	return vz.New(ctx, dir, paths)
}
