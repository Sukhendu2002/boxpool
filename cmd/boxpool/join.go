package main

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"boxpool.run/boxpool/internal/auth"
	"boxpool.run/boxpool/internal/client"
	"boxpool.run/boxpool/internal/daemon"
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
	writeNodes(os.Stdout, list, time.Now())
	return 0
}

func writeNodes(w io.Writer, list []daemon.Node, now time.Time) {
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "NAME\tROLE\tSTATE\tOS\tARCH\tFREE CPU\tFREE MEMORY\tLABELS\tJOINED\tWARNING")
	for _, n := range list {
		joined := "-"
		if !n.JoinedAt.IsZero() {
			joined = age(now.Sub(n.JoinedAt))
		}
		labels := cmp.Or(strings.Join(n.Labels, ","), "-")
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s %d/%s\t%s %s/%s\t%s\t%s\t%s\n", n.Name, n.Role, n.State, n.OS, n.Arch,
			capacityBar(int64(n.CPUs), int64(n.TotalCPUs)), n.CPUs, capacityTotal(int64(n.TotalCPUs), false),
			capacityBar(n.MemoryMB, n.TotalMemoryMB), mebibytes(int(n.MemoryMB)), capacityTotal(n.TotalMemoryMB, true), labels, joined, cmp.Or(n.Warning, "-"))
	}
	tw.Flush()
}

// Older nodes omit totals; do not turn unknown capacity into a full bar.
func capacityBar(free, total int64) string {
	if total <= 0 {
		return "[??????????]"
	}
	filled := int(float64(max(0, min(free, total))) / float64(total) * 10)
	return "[" + strings.Repeat("#", filled) + strings.Repeat("-", 10-filled) + "]"
}

func capacityTotal(n int64, memory bool) string {
	if n <= 0 {
		return "?"
	}
	if memory {
		return mebibytes(int(n))
	}
	return fmt.Sprint(n)
}
