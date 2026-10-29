package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"boxpool.run/boxpool/internal/auth"
	"boxpool.run/boxpool/internal/daemon"
)

const tokenUsage = `Usage:
  boxpool token create --join [--ttl 1h] [--transport direct|tailscale] [--addr host:port]
  boxpool token revoke <token>

create prints a token that boxpool join on another machine uses, once, to
join this machine's pool. It expires after --ttl. revoke makes an unused
token invalid.

--transport is how nodes reach this machine. direct, the default, names
this host's name on the LAN. tailscale names its tailnet address, so a node
behind another NAT joins with no port forwarding; it works with Headscale
too, and each node must be on the same tailnet. --addr overrides either.
`

func tokenCmd(args []string) int {
	if len(args) == 2 && args[0] == "revoke" {
		ctx := context.Background()
		c, ok := connect(ctx, "token revoke")
		if !ok {
			return 1
		}
		if err := c.RevokeJoinToken(ctx, args[1]); err != nil {
			fmt.Fprintf(os.Stderr, "boxpool token revoke: %v\n", err)
			return 1
		}
		return 0
	}
	if len(args) == 0 || args[0] != "create" {
		fmt.Fprint(os.Stderr, tokenUsage)
		return 2
	}
	fs := flag.NewFlagSet("token create", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, tokenUsage, "\nFlags:\n"); fs.PrintDefaults() }
	join := fs.Bool("join", false, "create a join token (the only kind for now)")
	ttl := fs.Duration("ttl", daemon.DefaultTokenTTL, "how long the token is valid")
	addr := fs.String("addr", "", "head address the token names, host:port (default: from --transport, port 7443)")
	transport := fs.String("transport", "direct", "how nodes reach this head: direct or tailscale")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if !*join || fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	if *transport != "direct" && *transport != "tailscale" {
		fmt.Fprintf(os.Stderr, "boxpool token create: unknown --transport %q: use direct or tailscale\n", *transport)
		return 2
	}
	if *transport == "tailscale" && *addr == "" {
		a, err := tailnetAddr()
		if err != nil {
			fmt.Fprintf(os.Stderr, "boxpool token create: %v\n", err)
			return 1
		}
		*addr = a
	}
	if *ttl <= 0 {
		fmt.Fprintf(os.Stderr, "boxpool token create: --ttl must be positive\n")
		return 2
	}
	ctx := context.Background()
	c, ok := connect(ctx, "token create")
	if !ok {
		return 1
	}
	tok, err := c.CreateJoinToken(ctx, daemon.TokenRequest{TTL: *ttl, Addr: *addr})
	if err != nil {
		fmt.Fprintf(os.Stderr, "boxpool token create: %v\n", err)
		return 1
	}
	fmt.Println(tok.Token)
	fmt.Fprintf(os.Stderr, "Single use; expires %s.\n", tok.ExpiresAt.Local().Format(time.DateTime))
	return 0
}

// tailnetAddr is this machine's pool address on its tailnet, from the
// system tailscale CLI. Nodes dial it through their own tailscaled.
func tailnetAddr() (string, error) {
	bin, err := exec.LookPath("tailscale")
	if err != nil && runtime.GOOS == "darwin" {
		bin, err = "/Applications/Tailscale.app/Contents/MacOS/Tailscale", nil // the Mac app does not put its CLI on PATH
	}
	if err != nil {
		return "", errors.New("the tailscale CLI is not installed: install Tailscale or Headscale's client, or pass --addr")
	}
	out, err := exec.Command(bin, "ip", "-1").Output()
	if err != nil {
		return "", fmt.Errorf("find this machine's tailnet address: tailscale ip -1: %w; run tailscale up, or pass --addr", err)
	}
	ip := net.ParseIP(strings.TrimSpace(string(out)))
	if ip == nil {
		return "", fmt.Errorf("tailscale ip -1 printed %q, not an address", out)
	}
	return net.JoinHostPort(ip.String(), auth.JoinPort), nil
}
