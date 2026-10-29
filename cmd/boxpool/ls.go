package main

import (
	"cmp"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
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
		fmt.Fprint(os.Stderr, "Usage: boxpool ls [--nodes] [--json]\n\nLists this machine's sandboxes, and on a head its nodes' sandboxes, or with\n--nodes the machines in its pool.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	asJSON := fs.Bool("json", false, "print a JSON array instead of a table")
	nodes := fs.Bool("nodes", false, "list the pool's machines, on the head")
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
	if *nodes {
		return lsNodes(ctx, c, *asJSON)
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

// writeTable prints list, with a NODE column on a head whose nodes have
// sandboxes; "-" is the head.
func writeTable(w io.Writer, list []daemon.Sandbox, now time.Time) {
	pool := slices.ContainsFunc(list, func(sb daemon.Sandbox) bool { return sb.Node != "" })
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	header := "NAME\tBACKEND\tOS\tSTATE\tCPU\tMEMORY\tAGE"
	if pool {
		header += "\tNODE"
	}
	fmt.Fprintln(tw, header)
	for _, sb := range list {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\t%s", sb.Name, sb.Backend, cmp.Or(sb.OS, "-"), sb.State, sb.CPUs, mebibytes(sb.MemoryMB), age(now.Sub(sb.CreatedAt)))
		if pool {
			fmt.Fprintf(tw, "\t%s", cmp.Or(sb.Node, "-"))
		}
		fmt.Fprintln(tw)
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

func start(args []string) int {
	return sandboxCmd("start", "Starts a stopped sandbox with the disk it had when it stopped.", (*client.Client).StartSandbox, args)
}

func stop(args []string) int {
	return sandboxCmd("stop", "Stops a sandbox. Its disk and its CPU, memory and disk reservation are kept.", (*client.Client).StopSandbox, args)
}

func pause(args []string) int {
	return sandboxCmd("pause", "Pauses a running sandbox: saves its memory and stops its VM until resume.\nOn Docker, pause stops the sandbox and its memory is lost.", (*client.Client).PauseSandbox, args)
}

func resume(args []string) int {
	return sandboxCmd("resume", "Resumes a paused sandbox where it left off. One that pause had to stop\nboots from its disk.", (*client.Client).ResumeSandbox, args)
}

// sandboxCmd runs a command that takes a sandbox name and calls the daemon
// once.
func sandboxCmd(cmd, help string, call func(*client.Client, context.Context, string) (daemon.Sandbox, error), args []string) int {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: boxpool %s <name>\n\n%s\n", cmd, help)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	ctx := context.Background()
	c, ok := connect(ctx, cmd)
	if !ok {
		return 1
	}
	sb, err := call(c, ctx, fs.Arg(0))
	return finish(cmd, sb, err)
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

// finish reports the result of a sandbox command, printing its warning.
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
