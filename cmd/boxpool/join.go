package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"boxpool.run/boxpool/internal/auth"
	"boxpool.run/boxpool/internal/client"
)

const joinUsage = `Usage: boxpool join <token>

Installs and starts the daemon as boxpool up does, then joins the pool of
the head that printed the token with boxpool token create --join. This
machine must reach the head's address in the token directly, over a LAN or
a tailnet.
`

func join(args []string) int {
	if len(args) != 1 || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(os.Stderr, joinUsage)
		return 2
	}
	// Before up, so a mistyped token installs nothing.
	if _, err := auth.ParseJoinToken(args[0]); err != nil {
		fmt.Fprintf(os.Stderr, "boxpool join: %v\n", err)
		return 2
	}
	if code := up(nil); code != 0 {
		return code
	}
	ctx := context.Background()
	c, ok := connect(ctx, "join")
	if !ok {
		return 1
	}
	res, err := c.JoinPool(ctx, args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "boxpool join: %v\n", err)
		return 1
	}
	fmt.Printf("joined the pool at %s as node %s\n", res.Head, res.ID)
	return 0
}

func lsNodes(ctx context.Context, c *client.Client, asJSON bool) int {
	list, err := c.ListNodes(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "boxpool ls: %v\n", err)
		return 1
	}
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(list)
		return 0
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "NAME\tROLE\tOS\tARCH\tJOINED")
	for _, n := range list {
		joined := "-"
		if !n.JoinedAt.IsZero() {
			joined = age(time.Since(n.JoinedAt))
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", n.Name, n.Role, n.OS, n.Arch, joined)
	}
	tw.Flush()
	return 0
}
