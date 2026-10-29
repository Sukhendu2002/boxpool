package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
)

const promoteUsage = `Usage: boxpool promote

Makes this machine its pool's head after the head is gone, from this
machine's replica of the pool's state. A pool of three or more machines
elects a new head by itself; a pool of two needs this. The daemon refuses
while it can still reach the head.
`

func promote(args []string) int {
	if len(args) != 0 {
		fmt.Fprint(os.Stderr, promoteUsage)
		return 2
	}
	fmt.Fprint(os.Stderr, "If the old head is only unreachable from here and still running, the pool\n"+
		"splits in two: each head serves the nodes that reach it, and changes on the\n"+
		"old head since this machine last heard from it are lost.\nType yes to promote this machine: ")
	answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if strings.TrimSpace(answer) != "yes" {
		fmt.Fprintln(os.Stderr, "boxpool promote: cancelled")
		return 1
	}
	ctx := context.Background()
	c, ok := connectLocal(ctx, "promote")
	if !ok {
		return 1
	}
	res, err := c.Promote(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "boxpool promote: %v\n", err)
		return 1
	}
	fmt.Printf("this machine is the pool's head at %s\n", res.Head)
	return 0
}
