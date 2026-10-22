//go:build !linux && !darwin

package agent

import (
	"context"
	"errors"
	"os"

	"github.com/coder/websocket"
)

func runExec(ctx context.Context, c *websocket.Conn) {
	execError(ctx, c, errors.New("exec is only supported on Linux and macOS"))
}

func lockPath(path string) (*os.File, error) {
	return nil, errors.New("unix sockets are only supported on Linux and macOS")
}
