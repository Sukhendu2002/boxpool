package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"text/tabwriter"
	"time"

	"boxpool.run/boxpool/internal/daemon"
)

const exposeUsage = `Usage:
  boxpool expose <sandbox> <port> [--public]
  boxpool expose ls
  boxpool expose rm <sandbox> <port>

On the head: prints an HTTPS URL, on its tailnet, for a TCP port in a
sandbox on any node of the pool. It needs Tailscale on the head, with
MagicDNS and HTTPS certificates enabled for the tailnet; open the URL
from any device on the tailnet, such as a phone with the Tailscale app.

The URL carries an expose token: opening it once sets a cookie, and
without either the URL answers 401. --public serves it with no token.
rm closes the URL and revokes its token.
`

func exposeCmd(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "ls":
			return exposeLs(args[1:])
		case "rm":
			return exposeRm(args[1:])
		}
	}
	fs := flag.NewFlagSet("expose", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, exposeUsage, "\nFlags:\n"); fs.PrintDefaults() }
	public := fs.Bool("public", false, "serve the URL to the tailnet without a token")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	pos := fs.Args()
	if len(pos) < 2 {
		fs.Usage()
		return 2
	}
	// Flags may also follow the arguments.
	if err := fs.Parse(pos[2:]); err != nil {
		return 2
	}
	port, err := strconv.Atoi(pos[1])
	if err != nil || fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	ctx := context.Background()
	c, ok := connectLocal(ctx, "expose")
	if !ok {
		return 1
	}
	e, err := c.Expose(ctx, daemon.ExposeRequest{Sandbox: pos[0], Port: port, Public: *public})
	if err != nil {
		fmt.Fprintf(os.Stderr, "boxpool expose: %v\n", err)
		return 1
	}
	fmt.Println(e.URL)
	if !e.Public {
		fmt.Fprintf(os.Stderr, "Anyone with this link opens the URL until boxpool expose rm %s %d.\n", e.Sandbox, e.Port)
	}
	return 0
}

func exposeLs(args []string) int {
	if len(args) != 0 {
		fmt.Fprint(os.Stderr, exposeUsage)
		return 2
	}
	ctx := context.Background()
	c, ok := connectLocal(ctx, "expose ls")
	if !ok {
		return 1
	}
	list, err := c.ListExposes(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "boxpool expose ls: %v\n", err)
		return 1
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "SANDBOX\tPORT\tURL\tAUTH\tAGE")
	for _, e := range list {
		access := "token " + e.TokenID
		if e.Public {
			access = "public"
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\n", e.Sandbox, e.Port, e.URL, access, age(time.Since(e.CreatedAt)))
	}
	tw.Flush()
	return 0
}

func exposeRm(args []string) int {
	if len(args) != 2 {
		fmt.Fprint(os.Stderr, exposeUsage)
		return 2
	}
	port, err := strconv.Atoi(args[1])
	if err != nil {
		fmt.Fprint(os.Stderr, exposeUsage)
		return 2
	}
	ctx := context.Background()
	c, ok := connectLocal(ctx, "expose rm")
	if !ok {
		return 1
	}
	if err := c.Unexpose(ctx, args[0], port); err != nil {
		fmt.Fprintf(os.Stderr, "boxpool expose rm: %v\n", err)
		return 1
	}
	return 0
}
