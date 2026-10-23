//go:build !linux

package daemon

import (
	"context"

	"boxpool.run/boxpool/internal/store"
)

// openDriver returns nil: the only backends so far need Linux.
func openDriver(ctx context.Context, dir string, info *Info, st *store.Store) (Driver, error) {
	return nil, nil
}
