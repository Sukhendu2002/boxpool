package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"boxpool.run/boxpool/internal/artifacts"
	"boxpool.run/boxpool/internal/backend/docker"
	"boxpool.run/boxpool/internal/backend/firecracker"
	"boxpool.run/boxpool/internal/store"
)

// NoNetwork is the warning for Firecracker sandboxes of a daemon that is not
// root, which cannot create their tap devices and firewall rules.
const NoNetwork = "sandboxes have no network: the daemon is not running as root"

// openDriver returns the driver for info.Backend, or nil for none. It may
// add to info.Warning.
func openDriver(ctx context.Context, dir string, info *Info, st *store.Store) (Driver, error) {
	switch info.Backend {
	case "firecracker":
		slog.Info("fetching Firecracker and the guest image; the first time downloads about 60 MB")
		start := time.Now()
		paths, err := artifacts.Fetch(ctx, filepath.Join(dir, artifacts.DirName))
		if err != nil {
			return nil, fmt.Errorf("fetch artifacts: %w", err)
		}
		slog.Info("artifacts ready", "took", time.Since(start).Round(time.Millisecond))
		if os.Geteuid() != 0 {
			st = nil // the driver's network needs root
			info.Warning = NoNetwork
		}
		d, err := firecracker.New(ctx, dir, paths, st)
		if err != nil {
			return nil, err
		}
		return d, nil
	case "docker":
		d, err := docker.New(ctx, dir)
		if err != nil {
			return nil, err
		}
		return d, nil
	}
	return nil, nil
}
