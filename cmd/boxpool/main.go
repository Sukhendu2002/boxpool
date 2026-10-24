// Command boxpool turns spare machines into a pool of agent sandboxes.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"boxpool.run/boxpool/internal/client"
	"boxpool.run/boxpool/internal/daemon"
	"boxpool.run/boxpool/internal/preflight"
)

var version = "0.0.0-dev"

const usage = `boxpool: a pool of agent sandboxes on machines you own

Usage:
  boxpool <command> [arguments]

Commands:
  daemon    run the node daemon in the foreground
  doctor    check whether this machine can host sandboxes
  version   print the version
  up        install and start the daemon: a systemd service on Linux (as
            root), a launch agent on macOS (as you)
  down      stop the daemon's service or launch agent, keeping its state
  join      join an existing pool (not implemented yet)
  sh        open a shell in a new or existing sandbox
  exec      run a command in a sandbox
  cp        copy files between this machine and a sandbox
  ls        list this machine's sandboxes
  stop      stop a sandbox
  rm        delete a sandbox and free its resources
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "daemon":
		os.Exit(runDaemon())
	case "doctor":
		os.Exit(doctor())
	case "version", "--version", "-v":
		fmt.Println("boxpool", version)
		if v := daemonVersion(); v != "" {
			fmt.Println("daemon ", v)
		}
	case "sh":
		os.Exit(sh(os.Args[2:]))
	case "exec":
		os.Exit(execCmd(os.Args[2:]))
	case "cp":
		os.Exit(cp(os.Args[2:]))
	case "ls":
		os.Exit(ls(os.Args[2:]))
	case "stop":
		os.Exit(stop(os.Args[2:]))
	case "rm":
		os.Exit(rm(os.Args[2:]))
	case "up":
		os.Exit(up(os.Args[2:]))
	case "down":
		os.Exit(down(os.Args[2:]))
	case "join":
		fmt.Fprintf(os.Stderr, "boxpool %s: not implemented yet\n", os.Args[1])
		os.Exit(1)
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "boxpool: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}

func doctor() int {
	r := preflight.Run()
	printReport(r)
	if !r.Ready() {
		return 1
	}
	return 0
}

func printReport(r preflight.Report) {
	fmt.Printf("boxpool doctor: %s/%s\n\n", r.OS, r.Arch)
	for _, c := range r.Checks {
		mark := "✗"
		if c.Warn {
			mark = "!"
		} else if c.OK {
			mark = "✓"
		}
		fmt.Printf("  %s %-30s %s\n", mark, c.Name, c.Detail)
	}
	fmt.Printf("\nBackend: %s\n", r.Backend)
	if r.Warning != "" {
		fmt.Printf("Warning: %s\n", r.Warning)
	}
}

func runDaemon() int {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	dir, err := preflight.StateDir()
	if err != nil {
		slog.Error("could not find state directory", "err", err)
		return 1
	}
	r := preflight.Run()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	info := daemon.Info{Version: version, OS: r.OS, Arch: r.Arch, StateDir: dir, Backend: r.Backend, Warning: r.Warning}
	if err := daemon.Run(ctx, dir, info); err != nil {
		slog.Error("daemon failed", "err", err)
		return 1
	}
	return 0
}

// daemonVersion returns the running daemon's version, or "" when none answers.
// Permission failures are reported rather than silently hiding a root daemon.
func daemonVersion() string {
	dir, err := preflight.StateDir()
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	info, err := client.New(dir).Info(ctx)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			fmt.Fprintf(os.Stderr, "boxpool version: %v\n", err)
		}
		return ""
	}
	return info.Version
}
