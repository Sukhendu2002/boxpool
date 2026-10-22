// Command boxpool turns spare machines into a pool of agent sandboxes.
package main

import (
	"fmt"
	"os"

	"boxpool.run/boxpool/internal/preflight"
)

var version = "0.0.0-dev"

const usage = `boxpool: a pool of agent sandboxes on machines you own

Usage:
  boxpool <command> [arguments]

Commands:
  doctor    check whether this machine can host sandboxes
  version   print the version
  up        start a pool on this machine (not implemented yet)
  join      join an existing pool (not implemented yet)
  sh        open a shell in a new sandbox (not implemented yet)
  ls        list nodes and sandboxes (not implemented yet)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "doctor":
		os.Exit(doctor())
	case "version", "--version", "-v":
		fmt.Println("boxpool", version)
	case "up", "join", "sh", "ls":
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
	if !r.Ready() {
		return 1
	}
	return 0
}
