//go:build !linux && !darwin

package agent

import (
	"context"
	"errors"

	"github.com/coder/websocket"
)

func runExec(ctx context.Context, c *websocket.Conn) {
	execError(ctx, c, errors.New("exec is only supported on Linux and macOS"))
}
