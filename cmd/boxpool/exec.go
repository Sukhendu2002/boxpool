package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/term"

	"boxpool.run/boxpool/internal/agent"
)

const execUsage = `Usage: boxpool exec [-i] [-t] <sandbox> [--] <command> [args...]

Runs a command in a running sandbox and exits with its exit code. Without
-t, its stdout and stderr stay separate and it gets no input unless -i.

Flags:
`

func execCmd(args []string) int {
	fs := flag.NewFlagSet("exec", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, execUsage); fs.PrintDefaults() }
	stdin := fs.Bool("i", false, "forward stdin to the command")
	tty := fs.Bool("t", false, "run the command on this terminal, in raw mode, as boxpool sh does (forwards stdin)")
	fs.BoolFunc("it", "same as -i -t", func(string) error { *stdin, *tty = true, true; return nil })
	if err := fs.Parse(args); err != nil {
		return 2
	}
	argv := fs.Args()
	if len(argv) > 1 && argv[1] == "--" {
		argv = append(argv[:1:1], argv[2:]...)
	}
	if len(argv) < 2 {
		fs.Usage()
		return 2
	}
	name, argv := argv[0], argv[1:]
	if *tty && !term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprintln(os.Stderr, "boxpool exec: -t needs a terminal on stdin")
		return 2
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGHUP, syscall.SIGTERM)
	defer cancel()
	c, ok := connect(ctx, "exec")
	if !ok {
		return 1
	}
	if *tty {
		code, err := attach(ctx, c, name, argv, os.Stdin, os.Stdout, os.Stderr, time.Time{}, false)
		if err != nil {
			fmt.Fprintf(os.Stderr, "boxpool exec: %v\n", err)
			return 1
		}
		return code
	}
	s, err := c.Exec(ctx, name, agent.ExecRequest{Argv: argv}, os.Stdout, os.Stderr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "boxpool exec: %v\n", err)
		return 1
	}
	if *stdin {
		// shortcut: this goroutine stays blocked reading stdin after the
		// command exits; fine for a command that exits next.
		go func() {
			if _, err := io.Copy(s, os.Stdin); err == nil {
				s.CloseStdin()
			}
		}()
	} else {
		s.CloseStdin() // as if from /dev/null
	}
	code, err := s.Wait()
	if err != nil {
		fmt.Fprintf(os.Stderr, "boxpool exec: %v\n", err)
		return 1
	}
	return code
}
