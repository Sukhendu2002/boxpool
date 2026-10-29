package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"boxpool.run/boxpool/internal/daemon"
)

const tokenUsage = `Usage:
  boxpool token create --join [--ttl 1h] [--addr host:port]
  boxpool token revoke <token>

create prints a token that boxpool join on another machine uses, once, to
join this machine's pool. It expires after --ttl. revoke makes an unused
token invalid.
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
	addr := fs.String("addr", "", "head address the token names, host:port (default: this host's name, port 7443)")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if !*join || fs.NArg() != 0 {
		fs.Usage()
		return 2
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
