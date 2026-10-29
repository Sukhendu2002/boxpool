package main

import (
	"context"
	"fmt"
	"os"
	"strings"
)

const labelUsage = `
--label sets this machine's labels, as boxpool node label does.
`

const nodeUsage = `Usage: boxpool node label LABEL,...

Replaces this machine's labels, which boxpool sh --node LABEL on the head
places sandboxes by, e.g. gpu,always-on. An empty list "" removes them.
The head sees the change at the next heartbeat.
`

func nodeCmd(args []string) int {
	if len(args) != 2 || args[0] != "label" {
		fmt.Fprint(os.Stderr, nodeUsage)
		return 2
	}
	return setLabels("node label", args[1])
}

// setLabels replaces this machine's labels with the comma-separated list.
func setLabels(cmd, list string) int {
	ctx := context.Background()
	c, ok := connect(ctx, cmd)
	if !ok {
		return 1
	}
	labels, err := c.SetLabels(ctx, strings.FieldsFunc(list, func(r rune) bool { return r == ',' }))
	if err != nil {
		fmt.Fprintf(os.Stderr, "boxpool %s: %v\n", cmd, err)
		return 1
	}
	fmt.Printf("labels: %s\n", strings.Join(labels, ","))
	return 0
}
