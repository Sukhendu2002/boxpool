package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

func logsCmd(args []string) int {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: boxpool logs [-f] <sandbox> | boxpool logs [-f] --daemon\n\nShows the current console log, or this node's daemon log. Flags precede\nthe sandbox name. Shell/exec output is not captured; write to /dev/console\ninside a VM to send output to its console.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	follow := fs.Bool("f", false, "follow new output until interrupted")
	daemon := fs.Bool("daemon", false, "read this node's daemon log")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if (*daemon && fs.NArg() != 0) || (!*daemon && fs.NArg() != 1) {
		fs.Usage()
		return 2
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	c, ok := connect(ctx, "logs")
	if !ok {
		return 1
	}
	logs, err := c.Logs(ctx, fs.Arg(0), *follow)
	if err == nil {
		defer logs.Close()
		_, err = io.Copy(os.Stdout, logs)
	}
	if err != nil && ctx.Err() == nil {
		fmt.Fprintf(os.Stderr, "boxpool logs: %v\n", err)
		return 1
	}
	return 0
}
