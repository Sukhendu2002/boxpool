//go:build darwin && cgo

package vz

import (
	"context"
	"io"

	"boxpool.run/boxpool/internal/backend"
)

func (d *Driver) Logs(ctx context.Context, id string, follow bool) (io.ReadCloser, error) {
	if err := validID(id); err != nil {
		return nil, err
	}
	return backend.OpenConsole(ctx, sandboxLayout(d.root, id).console, follow)
}
