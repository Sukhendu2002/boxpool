package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"

	"boxpool.run/boxpool/internal/agent"
	"boxpool.run/boxpool/internal/client"
	"boxpool.run/boxpool/internal/daemon"
	"boxpool.run/boxpool/internal/preflight"
)

// shell is the guest's login shell; the image is Debian, which has bash.
var shell = []string{"/bin/bash", "-l"}

const shUsage = `Usage: boxpool sh [flags] [name]

Opens a shell in a new sandbox, or with a name, in that existing one.
Leaving the shell leaves the sandbox running.

Flags:
`

func sh(args []string) int {
	start := time.Now()
	fs := flag.NewFlagSet("sh", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, shUsage); fs.PrintDefaults() }
	name := fs.String("name", "", "name for the sandbox, or of an existing one to attach to (default: generated)")
	cpu := fs.Int("cpu", 0, fmt.Sprintf("vCPUs of a new sandbox (default %d)", daemon.DefaultCPUs))
	mem := fs.String("mem", "", "memory of a new sandbox, e.g. 512m or 4g (default 2g)")
	disk := fs.String("disk", "", "disk of a new sandbox, e.g. 20g (default 10g)")
	timing := fs.Bool("timing", false, "print the time from the command's start to the shell's first output")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	req := daemon.CreateRequest{Name: *name, CPUs: *cpu}
	var err error
	if req.MemoryMB, err = parseSize(*mem, 1); err == nil {
		req.DiskGB, err = parseSize(*disk, 1024)
	}
	if err == nil && (fs.NArg() > 1 || fs.NArg() == 1 && *name != "") {
		err = errors.New("give at most one sandbox name")
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "boxpool sh: %v\n", err)
		return 2
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprintln(os.Stderr, "boxpool sh: stdin is not a terminal; to run commands from a script, use boxpool exec")
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGHUP, syscall.SIGTERM)
	defer stop()
	dir, err := preflight.StateDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "boxpool sh: %v\n", err)
		return 1
	}
	c := client.New(dir)
	info, err := c.Info(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "boxpool sh: cannot reach the daemon at %s (start it with boxpool daemon): %v\n", filepath.Join(dir, daemon.SocketName), err)
		return 1
	}
	if fs.NArg() == 1 {
		// Attaching: a typo must not create a new sandbox.
		if _, err := c.GetSandbox(ctx, fs.Arg(0)); err != nil {
			fmt.Fprintf(os.Stderr, "boxpool sh: %v\n", err)
			return 1
		}
		req = daemon.CreateRequest{Name: fs.Arg(0)}
	}
	if info.Warning != "" {
		fmt.Fprintf(os.Stderr, "warning: %s\n", info.Warning)
	}
	sb, err := c.CreateSandbox(ctx, req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "boxpool sh: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "sandbox %s (%s)\n", sb.Name, sb.Backend)
	code, err := attach(ctx, c, sb.Name, shell, os.Stdin, os.Stdout, os.Stderr, start, *timing)
	if err != nil {
		fmt.Fprintf(os.Stderr, "boxpool sh: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "sandbox %s is still running; reattach with: boxpool sh %s\n", sb.Name, sb.Name)
	return code
}

// attach runs argv in the sandbox on in's terminal, in raw mode, until it
// exits, and returns its exit code. The terminal is restored on every
// return and on panic.
func attach(ctx context.Context, c *client.Client, name string, argv []string, in *os.File, out, errOut io.Writer, start time.Time, timing bool) (int, error) {
	fd := int(in.Fd())
	cols, rows, err := term.GetSize(fd)
	if err != nil {
		return 0, err
	}
	req := agent.ExecRequest{Argv: argv, TTY: true, Rows: uint16(rows), Cols: uint16(cols),
		Env: []string{"TERM=" + cmp.Or(os.Getenv("TERM"), "xterm-256color")}}
	if timing {
		out = &firstWrite{w: out, f: func() {
			fmt.Fprintf(errOut, "time to prompt: %v\r\n", time.Since(start).Round(time.Millisecond))
		}}
	}
	old, err := term.MakeRaw(fd)
	if err != nil {
		return 0, err
	}
	defer term.Restore(fd, old)

	// Before the handshake, so a resize during it is not lost.
	winch := make(chan os.Signal, 1)
	notifyResize(winch)
	defer signal.Stop(winch)
	s, err := c.Exec(ctx, name, req, out, out)
	if err != nil {
		return 0, err
	}
	resize := func() {
		if cols, rows, err := term.GetSize(fd); err == nil {
			s.Resize(uint16(rows), uint16(cols))
		}
	}
	resize() // the size may have changed since req was built
	go func() {
		for range winch {
			resize()
		}
	}()
	// shortcut: this goroutine stays blocked reading in after the session
	// ends; fine for a command that exits next.
	go io.Copy(s, in)

	code, err := s.Wait()
	if err != nil {
		return 0, fmt.Errorf("connection to sandbox %s lost: %w", name, err)
	}
	return code, nil
}

// firstWrite calls f before the first write to w.
type firstWrite struct {
	w    io.Writer
	f    func()
	once sync.Once
}

func (fw *firstWrite) Write(p []byte) (int, error) {
	fw.once.Do(fw.f)
	return fw.w.Write(p)
}

// parseSize parses a size such as 512m, 4g or 2048 (MiB) and returns it in
// units of unit MiB, which it must be a whole number of. "" is 0.
func parseSize(s string, unit int) (int, error) {
	if s == "" {
		return 0, nil
	}
	num, mul := strings.ToLower(s), 1
	switch {
	case strings.HasSuffix(num, "g"):
		num, mul = num[:len(num)-1], 1024
	case strings.HasSuffix(num, "m"):
		num = num[:len(num)-1]
	}
	n, err := strconv.Atoi(num)
	if err != nil || n < 1 || n*mul%unit != 0 {
		if unit == 1024 {
			return 0, fmt.Errorf("bad size %q: want whole gigabytes, e.g. 20g", s)
		}
		return 0, fmt.Errorf("bad size %q: want e.g. 512m or 4g", s)
	}
	return n * mul / unit, nil
}
