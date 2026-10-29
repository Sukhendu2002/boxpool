package transport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Tailscale uses system tailscaled (including clients managed by Headscale).
// It shares Direct's sockets: the OS routes tailnet addresses through the
// installed client. Userspace-only tailscaled networking is not supported.
type Tailscale struct{ Direct }

func (Tailscale) Address(ctx context.Context, port string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	bin, err := exec.LookPath("tailscale")
	if err != nil && runtime.GOOS == "darwin" {
		bin, err = "/Applications/Tailscale.app/Contents/MacOS/Tailscale", nil
	}
	if err != nil {
		return "", errors.New("the tailscale CLI is not installed: install Tailscale or Headscale's client, or pass --addr")
	}
	cmd := exec.CommandContext(ctx, bin, "ip", "-1")
	// A wrapper can leave a child holding stdout after it is killed.
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return "", fmt.Errorf("find this machine's tailnet address: tailscale ip -1: %w; run tailscale up, or pass --addr", err)
	}
	ip := net.ParseIP(strings.TrimSpace(string(out)))
	if ip == nil {
		return "", fmt.Errorf("tailscale ip -1 printed %q, not an address", out)
	}
	return net.JoinHostPort(ip.String(), port), nil
}
