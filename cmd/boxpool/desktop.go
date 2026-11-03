package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"

	"boxpool.run/boxpool/internal/daemon"
)

const desktopUsage = `Usage: boxpool desktop [--no-open] <sandbox>
       boxpool desktop rm <sandbox>

On the head: starts the sandbox's desktop, prints an authenticated HTTPS
viewer URL and opens it in your browser. Requires the desktop image and the
same Tailscale setup as expose. The viewer starts read-only; Take over enables
mouse and keyboard. rm closes the URL and revokes its token.
`

var openDesktopURL = func(url string) error {
	command := "xdg-open"
	if runtime.GOOS == "darwin" {
		command = "open"
	}
	return exec.Command(command, url).Run()
}

func desktopCmd(args []string) int {
	remove := len(args) > 0 && args[0] == "rm"
	if remove {
		args = args[1:]
	}
	fs := flag.NewFlagSet("desktop", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, desktopUsage) }
	noOpen := fs.Bool("no-open", false, "print the URL without opening a browser")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	name := fs.Arg(0)
	ctx := context.Background()
	c, ok := connectLocal(ctx, "desktop")
	if !ok {
		return 1
	}
	if remove {
		if err := c.Unexpose(ctx, name, daemon.DesktopPort); err != nil {
			fmt.Fprintf(os.Stderr, "boxpool desktop: %v\n", err)
			return 1
		}
		return 0
	}
	if _, err := c.StartDesktop(ctx, name); err != nil {
		fmt.Fprintf(os.Stderr, "boxpool desktop: %v\n", err)
		return 1
	}
	e, err := c.Expose(ctx, daemon.ExposeRequest{Sandbox: name, Desktop: true})
	if err != nil {
		fmt.Fprintf(os.Stderr, "boxpool desktop: %v (use boxpool desktop rm %s to close an existing viewer URL)\n", err, name)
		return 1
	}
	fmt.Println(e.URL)
	fmt.Fprintf(os.Stderr, "Anyone with this link can view and take control until boxpool desktop rm %s.\n", name)
	if !*noOpen {
		if err := openDesktopURL(e.URL); err != nil {
			fmt.Fprintf(os.Stderr, "boxpool desktop: open the URL above in a browser (%v)\n", err)
		}
	}
	return 0
}
