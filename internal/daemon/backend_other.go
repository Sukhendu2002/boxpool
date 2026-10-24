//go:build !linux && (!darwin || !cgo)

package daemon

import (
	"context"
	"fmt"

	"boxpool.run/boxpool/internal/store"
)

// openDriver reports a missing cgo build when macOS selected virtualization.
func openDriver(ctx context.Context, dir string, info *Info, st *store.Store) (Driver, error) {
	if info.Backend == "virtualization framework" {
		return nil, fmt.Errorf("the Virtualization framework backend requires a macOS build with CGO_ENABLED=1")
	}
	return nil, nil
}
