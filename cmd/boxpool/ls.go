package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"text/tabwriter"
	"time"

	"boxpool.run/boxpool/internal/client"
	"boxpool.run/boxpool/internal/daemon"
	"boxpool.run/boxpool/internal/preflight"
)

// connect returns a client for the local daemon, or prints why there is
// none for the command cmd.
func connect(ctx context.Context, cmd string) (*client.Client, bool) {
	dir, err := preflight.StateDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "boxpool %s: %v\n", cmd, err)
		return nil, false
	}
	c := client.New(dir)
	if err := c.Health(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "boxpool %s: %v\n", cmd, err)
		return nil, false
	}
	return c, true
}

func ls(args []string) int {
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: boxpool ls [--json]\n\nLists this machine's sandboxes.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	asJSON := fs.Bool("json", false, "print a JSON array instead of a table")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	ctx := context.Background()
	c, ok := connect(ctx, "ls")
	if !ok {
		return 1
	}
	list, err := c.ListSandboxes(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "boxpool ls: %v\n", err)
		return 1
	}
	if *asJSON {
		writeJSONList(os.Stdout, list)
	} else {
		writeTable(os.Stdout, list, time.Now())
	}
	return 0
}

func writeJSONList(w io.Writer, list []daemon.Sandbox) {
	if list == nil {
		list = []daemon.Sandbox{}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(list)
}

func writeTable(w io.Writer, list []daemon.Sandbox, now time.Time) {
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "NAME\tBACKEND\tSTATE\tCPU\tMEMORY\tAGE")
	for _, sb := range list {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\n", sb.Name, sb.Backend, sb.State, sb.CPUs, mebibytes(sb.MemoryMB), age(now.Sub(sb.CreatedAt)))
	}
	tw.Flush()
}

// mebibytes formats n MiB as boxpool sh's --mem takes it.
func mebibytes(n int) string {
	if n%1024 == 0 {
		return strconv.Itoa(n/1024) + "g"
	}
	return strconv.Itoa(n) + "m"
}

// age formats d in its largest whole unit: 45s, 12m, 5h, 3d.
func age(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", max(0, int(d.Seconds())))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func stop(args []string) int {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: boxpool stop <name>\n\nStops a sandbox. Its disk and its CPU, memory and disk reservation are kept.\n")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	ctx := context.Background()
	c, ok := connect(ctx, "stop")
	if !ok {
		return 1
	}
	sb, err := c.StopSandbox(ctx, fs.Arg(0))
	return finish("stop", sb, err)
}

func rm(args []string) int {
	fs := flag.NewFlagSet("rm", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: boxpool rm [--force] <name>\n\nDeletes a stopped sandbox and frees its resources: process, disk, network\nand reservation.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	force := fs.Bool("force", false, "stop the sandbox first if it is running")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	ctx := context.Background()
	c, ok := connect(ctx, "rm")
	if !ok {
		return 1
	}
	sb, err := c.RemoveSandbox(ctx, fs.Arg(0), *force)
	return finish("rm", sb, err)
}

// finish reports the result of stop or rm. An unclean stop is a warning.
func finish(cmd string, sb daemon.Sandbox, err error) int {
	if err != nil {
		fmt.Fprintf(os.Stderr, "boxpool %s: %v\n", cmd, err)
		return 1
	}
	if sb.Warning != "" {
		fmt.Fprintf(os.Stderr, "warning: %s\n", sb.Warning)
	}
	return 0
}
