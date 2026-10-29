package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"
)

const snapshotUsage = `Usage:
  boxpool snapshot <sandbox> --name <snapshot>
  boxpool snapshot ls [--json]
  boxpool snapshot rm <snapshot>

Saves a copy of a sandbox's disk; boxpool sh --from <snapshot> starts new
sandboxes from it, on this machine. A running sandbox is stopped for the
copy and started again, so its processes are restarted. A paused one is
refused.
`

func snapshotCmd(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "ls":
			return snapshotLs(args[1:])
		case "rm":
			return snapshotRm(args[1:])
		}
	}
	fs := flag.NewFlagSet("snapshot", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, snapshotUsage, "\nFlags:\n"); fs.PrintDefaults() }
	name := fs.String("name", "", "name for the snapshot")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	// Flags may also follow the sandbox's name.
	sandbox := fs.Arg(0)
	if err := fs.Parse(fs.Args()[min(1, fs.NArg()):]); err != nil {
		return 2
	}
	if sandbox == "" || *name == "" || fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	ctx := context.Background()
	c, ok := connect(ctx, "snapshot")
	if !ok {
		return 1
	}
	snap, err := c.SnapshotSandbox(ctx, sandbox, *name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "boxpool snapshot: %v\n", err)
		return 1
	}
	if snap.Warning != "" {
		fmt.Fprintf(os.Stderr, "warning: %s\n", snap.Warning)
	}
	return 0
}

func snapshotLs(args []string) int {
	fs := flag.NewFlagSet("snapshot ls", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, snapshotUsage, "\nFlags:\n"); fs.PrintDefaults() }
	asJSON := fs.Bool("json", false, "print a JSON array instead of a table")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	ctx := context.Background()
	c, ok := connect(ctx, "snapshot ls")
	if !ok {
		return 1
	}
	list, err := c.ListSnapshots(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "boxpool snapshot ls: %v\n", err)
		return 1
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(list)
		return 0
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSANDBOX\tBACKEND\tDISK\tAGE")
	for _, snap := range list {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%dg\t%s\n", snap.Name, snap.Sandbox, snap.Backend, snap.DiskGB, age(time.Since(snap.CreatedAt)))
	}
	tw.Flush()
	return 0
}

func snapshotRm(args []string) int {
	if len(args) != 1 {
		fmt.Fprint(os.Stderr, snapshotUsage)
		return 2
	}
	ctx := context.Background()
	c, ok := connect(ctx, "snapshot rm")
	if !ok {
		return 1
	}
	if _, err := c.RemoveSnapshot(ctx, args[0]); err != nil {
		fmt.Fprintf(os.Stderr, "boxpool snapshot rm: %v\n", err)
		return 1
	}
	return 0
}
