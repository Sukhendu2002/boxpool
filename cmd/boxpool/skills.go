package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"boxpool.run/boxpool/internal/skills"
)

const skillsUsage = `Usage: boxpool skills install [--dir <skills-directory>] [--force]

Installs the bundled boxpool-workspace skill for an agent running on this
machine and working in a sandbox. No daemon or model login is needed.
The default skills directory is ~/.agents/skills (Pi and Codex). For
Claude Code, use --dir ~/.claude/skills. Restart or reload your harness
after installation and explicitly select the Boxpool workspace skill.

Existing identical content is kept. Different content is preserved unless
--force is given. Only boxpool-workspace/SKILL.md is written.
`

func skillsCmd(args []string) int {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Fprint(os.Stdout, skillsUsage)
		return 0
	}
	if len(args) == 0 || args[0] != "install" {
		fmt.Fprint(os.Stderr, skillsUsage)
		return 2
	}
	fs := flag.NewFlagSet("skills install", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, skillsUsage+"\nFlags:\n"); fs.PrintDefaults() }
	dir := fs.String("dir", "", "skills directory (default ~/.agents/skills)")
	force := fs.Bool("force", false, "replace different existing Boxpool skill instructions")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	dirProvided := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "dir" {
			dirProvided = true
		}
	})
	if dirProvided && strings.TrimSpace(*dir) == "" {
		fmt.Fprintln(os.Stderr, "boxpool skills: --dir must not be empty")
		return 2
	}
	if *dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintf(os.Stderr, "boxpool skills: %v; specify --dir\n", err)
			return 1
		}
		*dir = filepath.Join(home, ".agents", "skills")
	}
	path, err := skills.Install(*dir, *force)
	if err != nil {
		fmt.Fprintf(os.Stderr, "boxpool skills: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stdout, "Boxpool workspace skill ready at %s\nRestart or reload your harness, then use boxpool-workspace with a sandbox and guest project path.\n", path)
	return 0
}
