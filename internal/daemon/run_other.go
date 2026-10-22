//go:build !linux && !darwin

package daemon

import (
	"context"
	"errors"
)

// Run is only supported on Linux and macOS.
func Run(ctx context.Context, dir string, info Info) error {
	return errors.New("unsupported OS")
}
