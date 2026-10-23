package main

import (
	"archive/tar"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"boxpool.run/boxpool/internal/agent"
)

const cpUsage = `Usage: boxpool cp SRC DST

Copies a file or directory between this machine and a sandbox. Exactly one
of SRC and DST is <sandbox>:<absolute path>; the other is a path here.
Modes, modification times and symlinks are kept, owners are not.

  boxpool cp ./dir box:/tmp/      copies ./dir to /tmp/dir in box, which
                                  must be an existing directory
  boxpool cp box:/tmp/dir ./out   copies it to ./out/dir if ./out is a
                                  directory, else to ./out
`

func cp(args []string) int {
	fs := flag.NewFlagSet("cp", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, cpUsage) }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return 2
	}
	name, src, dst, upload, err := parseCopy(fs.Arg(0), fs.Arg(1))
	if err != nil {
		fmt.Fprintf(os.Stderr, "boxpool cp: %v\n", err)
		return 2
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGHUP, syscall.SIGTERM)
	defer cancel()
	c, ok := connect(ctx, "cp")
	if !ok {
		return 1
	}
	if upload {
		if _, err := os.Lstat(src); err != nil {
			fmt.Fprintf(os.Stderr, "boxpool cp: %v\n", err)
			return 1
		}
		pr, pw := io.Pipe()
		go func() { pw.CloseWithError(agent.WriteTar(pw, src)) }()
		err = c.Upload(ctx, name, dst, pr)
		pr.CloseWithError(errors.New("upload ended")) // unblocks WriteTar if the upload failed early
	} else {
		var tr io.ReadCloser
		if tr, err = c.Download(ctx, name, src); err == nil {
			err = extractDownload(tr, src, dst)
			if err == nil {
				// An aborted transfer can surface only after the tar's end.
				_, err = io.Copy(io.Discard, tr)
			}
			tr.Close()
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "boxpool cp: %v\n", err)
		return 1
	}
	return 0
}

// sandboxName matches the daemon's rule for names.
var sandboxName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`).MatchString

// parseCopy parses cp's arguments. Exactly one must be <sandbox>:<path>,
// with an absolute path; it is the source when upload is false. An
// argument is a host path if it starts with / or ., has no colon, or has a
// slash before its first colon, so ./a:b and /x:y are host paths.
func parseCopy(srcArg, dstArg string) (name, src, dst string, upload bool, err error) {
	split := func(arg string) (name, p string, remote bool, err error) {
		before, after, found := strings.Cut(arg, ":")
		if !found || strings.HasPrefix(arg, "/") || strings.HasPrefix(arg, ".") || strings.Contains(before, "/") {
			if arg == "" {
				return "", "", false, errors.New("empty path")
			}
			return "", arg, false, nil
		}
		if !sandboxName(before) {
			return "", "", false, fmt.Errorf("invalid sandbox name %q in %q (to copy a local file with a colon in its name, write ./%s)", before, arg, arg)
		}
		if !path.IsAbs(after) {
			return "", "", false, fmt.Errorf("the path in %q must be absolute, e.g. %s:/root/%s", arg, before, after)
		}
		return before, after, true, nil
	}
	srcName, src, srcRemote, err := split(srcArg)
	if err != nil {
		return
	}
	dstName, dst, dstRemote, err := split(dstArg)
	if err != nil {
		return
	}
	switch {
	case srcRemote && dstRemote:
		err = errors.New("copying between sandboxes is not supported; copy through this machine")
	case !srcRemote && !dstRemote:
		err = errors.New("one of SRC and DST must be <sandbox>:<path>")
	case srcRemote && path.Clean(src) == "/":
		err = errors.New("cannot copy a sandbox's whole file system; name a directory in it")
	case srcRemote:
		name = srcName
	default:
		name, upload = dstName, true
	}
	return name, src, dst, upload, err
}

// extractDownload extracts tr, the guest's tar of src, as cp's usage says:
// into dst if it is a directory, else as dst. The guest is untrusted, so on
// top of ExtractTar's checks every entry must be src's base name or below
// it, and set-user-ID and set-group-ID bits are dropped: a sandbox can only
// write the copy that was asked for, and no privileged program.
func extractDownload(tr io.Reader, src, dst string) error {
	want := path.Base(path.Clean(src))
	dest, rename := dst, ""
	if info, err := os.Stat(dst); err != nil || !info.IsDir() {
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		dest, rename = filepath.Dir(dst), filepath.Base(dst)
	}
	n := 0
	err := agent.ExtractTar(tr, dest, func(h *tar.Header) error {
		n++
		name := path.Clean(h.Name)
		if name != want && !strings.HasPrefix(name, want+"/") {
			return fmt.Errorf("not in %s: refusing a sandbox's write outside the copy", want)
		}
		if rename != "" {
			name = rename + strings.TrimPrefix(name, want)
		}
		h.Name = name
		h.Mode &^= 0o6000
		return nil
	})
	if err == nil && n == 0 {
		err = errors.New("the sandbox sent an empty archive")
	}
	return err
}
