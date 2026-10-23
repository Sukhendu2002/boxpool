//go:build linux || darwin

package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"boxpool.run/boxpool/internal/client"
	"boxpool.run/boxpool/internal/daemon"
)

func TestParseCopy(t *testing.T) {
	for _, c := range []struct {
		src, dst       string
		name, from, to string
		upload         bool
		err            string
	}{
		{src: "./dir", dst: "box:/tmp/", name: "box", from: "./dir", to: "/tmp/", upload: true},
		{src: "box:/tmp/dir", dst: "./out", name: "box", from: "/tmp/dir", to: "./out"},
		{src: "/abs/file", dst: "sb-1a2b3c:/root", name: "sb-1a2b3c", from: "/abs/file", to: "/root", upload: true},
		{src: "dir", dst: "box:/x", name: "box", from: "dir", to: "/x", upload: true},
		{src: "./a:b", dst: "box:/x", name: "box", from: "./a:b", to: "/x", upload: true},
		{src: "/x:y", dst: "box:/x", name: "box", from: "/x:y", to: "/x", upload: true},
		{src: "a/b:c", dst: "box:/x", name: "box", from: "a/b:c", to: "/x", upload: true},
		{src: "box:/a:b", dst: ".", name: "box", from: "/a:b", to: "."},
		{src: "box:/tmp/dir", dst: "out:/x", err: "between sandboxes"},
		{src: "x", dst: "y", err: "one of SRC and DST"},
		{src: "box:tmp", dst: ".", err: "must be absolute"},
		{src: "box:", dst: ".", err: "must be absolute"},
		{src: "Box:/x", dst: ".", err: "invalid sandbox name"},
		{src: `C:\x`, dst: "box:/y", err: "invalid sandbox name"},
		{src: "a b:/x", dst: ".", err: "invalid sandbox name"},
		{src: "box:/", dst: ".", err: "whole file system"},
		{src: "box:/tmp/..", dst: ".", err: "whole file system"},
		{src: "", dst: "box:/x", err: "empty path"},
	} {
		name, from, to, upload, err := parseCopy(c.src, c.dst)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("parseCopy(%q, %q) = %v, want an error with %q", c.src, c.dst, err, c.err)
			}
			continue
		}
		if err != nil || name != c.name || from != c.from || to != c.to || upload != c.upload {
			t.Errorf("parseCopy(%q, %q) = %q %q %q %v %v", c.src, c.dst, name, from, to, upload, err)
		}
	}
}

// tarOf returns a tar of headers; regular files get their names as content.
func tarOf(t *testing.T, headers ...*tar.Header) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, h := range headers {
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(h.Name))
		}
		if h.Mode == 0 {
			h.Mode = 0644
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			tw.Write([]byte(h.Name))
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return &b
}

// TestExtractDownloadSafety feeds hostile archives, as a compromised guest
// could send for boxpool cp box:/tmp/dir, and checks that nothing outside
// the requested copy is written.
func TestExtractDownloadSafety(t *testing.T) {
	dir := &tar.Header{Name: "dir", Typeflag: tar.TypeDir, Mode: 0755}
	for _, c := range []struct {
		name    string
		headers func(outside string) []*tar.Header
		err     string
	}{
		{"absolute", func(string) []*tar.Header {
			return []*tar.Header{dir, {Name: "/tmp/abs", Typeflag: tar.TypeReg}}
		}, "outside the copy"},
		{"dot-dot", func(string) []*tar.Header {
			return []*tar.Header{dir, {Name: "dir/../../escape", Typeflag: tar.TypeReg}}
		}, "outside the copy"},
		{"sibling of the copy", func(string) []*tar.Header {
			return []*tar.Header{dir, {Name: ".bashrc", Typeflag: tar.TypeReg}}
		}, "outside the copy"},
		{"dot", func(string) []*tar.Header {
			return []*tar.Header{{Name: ".", Typeflag: tar.TypeDir, Mode: 0777}}
		}, "outside the copy"},
		{"through a symlink", func(outside string) []*tar.Header {
			return []*tar.Header{dir, {Name: "dir/link", Typeflag: tar.TypeSymlink, Linkname: outside}, {Name: "dir/link/pwned", Typeflag: tar.TypeReg}}
		}, "symlink parents are not allowed"},
		{"root is a symlink", func(outside string) []*tar.Header {
			return []*tar.Header{{Name: "dir", Typeflag: tar.TypeSymlink, Linkname: outside}, {Name: "dir/pwned", Typeflag: tar.TypeReg}}
		}, "symlink parents are not allowed"},
		{"hard link", func(outside string) []*tar.Header {
			return []*tar.Header{dir, {Name: "dir/h", Typeflag: tar.TypeLink, Linkname: filepath.Join(outside, "sentinel")}}
		}, "unsupported tar type"},
		{"device", func(string) []*tar.Header {
			return []*tar.Header{dir, {Name: "dir/null", Typeflag: tar.TypeChar, Devmajor: 1, Devminor: 3}}
		}, "unsupported tar type"},
		{"fifo", func(string) []*tar.Header {
			return []*tar.Header{dir, {Name: "dir/fifo", Typeflag: tar.TypeFifo}}
		}, "unsupported tar type"},
		{"empty", func(string) []*tar.Header { return nil }, "empty archive"},
	} {
		t.Run(c.name, func(t *testing.T) {
			tmp := t.TempDir()
			outside, dst := filepath.Join(tmp, "outside"), filepath.Join(tmp, "dst")
			for _, d := range []string{outside, dst} {
				if err := os.Mkdir(d, 0755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(outside, "sentinel"), []byte("keep"), 0644); err != nil {
				t.Fatal(err)
			}
			before := snapshot(t, tmp, dst)
			err := extractDownload(tarOf(t, c.headers(outside)...), "/tmp/dir", dst)
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Fatalf("extractDownload = %v, want an error with %q", err, c.err)
			}
			if after := snapshot(t, tmp, dst); after != before {
				t.Errorf("wrote outside the copy:\nbefore %s\nafter  %s", before, after)
			}
			if fi, err := os.Stat(dst); err != nil || fi.Mode().Perm() != 0755 {
				t.Errorf("destination changed: %v, %v", fi.Mode(), err)
			}
		})
	}
}

// snapshot lists everything under root except dst/dir, the copy.
func snapshot(t *testing.T, root, dst string) string {
	t.Helper()
	var b strings.Builder
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			t.Fatal(err)
		}
		if p == filepath.Join(dst, "dir") {
			return fs.SkipDir
		}
		info, _ := d.Info()
		content, _ := os.ReadFile(p)
		b.WriteString(p + " " + info.Mode().String() + " " + string(content) + "; ")
		return nil
	})
	return b.String()
}

func TestExtractDownload(t *testing.T) {
	tmp := t.TempDir()
	outside := filepath.Join(tmp, "outside")
	os.WriteFile(outside, []byte("keep"), 0644)
	mtime := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	archive := func() *bytes.Buffer {
		return tarOf(t,
			&tar.Header{Name: "dir/", Typeflag: tar.TypeDir, Mode: 0750, ModTime: mtime},
			&tar.Header{Name: "dir/suid", Typeflag: tar.TypeReg, Mode: 0o6755, ModTime: mtime},
			&tar.Header{Name: "dir/out", Typeflag: tar.TypeSymlink, Linkname: outside},
			// Replaces the symlink; does not write through it.
			&tar.Header{Name: "dir/out", Typeflag: tar.TypeReg, Mode: 0600},
		)
	}

	// Into an existing directory.
	if err := extractDownload(archive(), "/tmp/dir/", tmp); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(outside); string(b) != "keep" {
		t.Errorf("wrote through a symlink: %q", b)
	}
	fi, err := os.Lstat(filepath.Join(tmp, "dir", "suid"))
	if err != nil || fi.Mode() != 0755 || !fi.ModTime().Equal(mtime) {
		t.Errorf("suid = %v %v, %v; want 0755 without set-ID bits, %v", fi.Mode(), fi.ModTime(), err, mtime)
	}
	if fi, err := os.Stat(filepath.Join(tmp, "dir")); err != nil || fi.Mode().Perm() != 0750 || !fi.ModTime().Equal(mtime) {
		t.Errorf("dir = %v, %v", fi, err)
	}
	if fi, err := os.Lstat(filepath.Join(tmp, "dir", "out")); err != nil || !fi.Mode().IsRegular() {
		t.Errorf("out = %v, %v; want a regular file", fi, err)
	}

	// As a new name.
	if err := extractDownload(archive(), "/tmp/dir", filepath.Join(tmp, "copy")); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(tmp, "copy", "suid")); err != nil || string(b) != "dir/suid" {
		t.Errorf("copy/suid = %q, %v", b, err)
	}
	// A file over an existing file.
	if err := extractDownload(tarOf(t, &tar.Header{Name: "hostname", Typeflag: tar.TypeReg}), "/etc/hostname", outside); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(outside); string(b) != "hostname" {
		t.Errorf("file copy = %q", b)
	}
	// The parent of a new name must exist.
	if err := extractDownload(archive(), "/tmp/dir", filepath.Join(tmp, "no", "copy")); err == nil {
		t.Error("extracted into a missing parent")
	}
}

func TestExecAndCp(t *testing.T) {
	dir := testDaemon(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := client.New(dir).CreateSandbox(ctx, daemon.CreateRequest{Name: "box"}); err != nil {
		t.Fatal(err)
	}

	t.Run("exec", func(t *testing.T) {
		code, out, errOut := cli(t, dir, "", "exec", "box", "--", "sh", "-c", "echo out; echo err >&2; exit 7")
		if code != 7 || out != "out\n" || errOut != "err\n" {
			t.Errorf("exec = %d %q %q", code, out, errOut)
		}
		if code, out, _ := cli(t, dir, "hi\n", "exec", "-i", "box", "--", "cat"); code != 0 || out != "hi\n" {
			t.Errorf("exec -i cat = %d %q", code, out)
		}
		// Without -i the command sees an empty stdin, not this one.
		if code, out, _ := cli(t, dir, "hi\n", "exec", "box", "cat"); code != 0 || out != "" {
			t.Errorf("exec cat = %d %q", code, out)
		}
		// The command's own codes pass through; 125 collides, as in docker.
		for _, want := range []int{1, 125} {
			if code, _, _ := cli(t, dir, "", "exec", "box", "sh", "-c", fmt.Sprintf("exit %d", want)); code != want {
				t.Errorf("exec of exit %d = %d", want, code)
			}
		}
		if code, _, errOut := cli(t, dir, "", "exec", "nope", "--", "true"); code != execFailed || !strings.Contains(errOut, `sandbox "nope" not found`) {
			t.Errorf("exec nope = %d %q", code, errOut)
		}
		for _, args := range [][]string{{"exec"}, {"exec", "box"}, {"exec", "box", "--"}, {"exec", "-t", "box", "true"}, {"exec", "-x", "box", "true"}} {
			if code, _, _ := cli(t, dir, "", args...); code != execFailed {
				t.Errorf("%q = %d, want %d", args, code, execFailed)
			}
		}
	})

	t.Run("exec -t", func(t *testing.T) {
		r, tm := boxpool(t, dir, "exec", "-t", "box", "--", "sh", "-c", "stty size; exit 4")
		tm.expect("24 80")
		r.wait(t, 4)
	})

	t.Run("cp round trip", func(t *testing.T) {
		// The fake sandbox's agent runs here, so its paths are local too.
		host, guest := t.TempDir(), t.TempDir()
		src := filepath.Join(host, "dir")
		mtime := time.Date(2021, 6, 7, 8, 9, 10, 123456789, time.UTC)
		for _, d := range []string{"dir", "dir/sub", "dir/empty"} {
			os.Mkdir(filepath.Join(host, d), 0755)
		}
		os.WriteFile(filepath.Join(src, "a.txt"), []byte("hello\n"), 0644)
		os.WriteFile(filepath.Join(src, "sub", "run.sh"), bytes.Repeat([]byte("#!/bin/sh\n"), 50000), 0755)
		os.Symlink("../a.txt", filepath.Join(src, "sub", "link"))
		os.Chtimes(filepath.Join(src, "a.txt"), mtime, mtime)

		if code, _, errOut := cli(t, dir, "", "cp", src, "box:"+guest+"/"); code != 0 {
			t.Fatalf("cp to the sandbox = %d %q", code, errOut)
		}
		out := filepath.Join(host, "out")
		if code, _, errOut := cli(t, dir, "", "cp", "box:"+guest+"/dir", out); code != 0 {
			t.Fatalf("cp from the sandbox = %d %q", code, errOut)
		}
		if want, got := tree(t, src), tree(t, out); got != want {
			t.Errorf("round trip differs:\n got %s\nwant %s", got, want)
		}
		if code, _, errOut := cli(t, dir, "", "cp", "box:"+guest+"/missing", out); code != 1 || !strings.Contains(errOut, "no such file or directory") {
			t.Errorf("cp of a missing file = %d %q", code, errOut)
		}
		if code, _, errOut := cli(t, dir, "", "cp", src, "box:"+guest+"/missing/"); code != 1 || !strings.Contains(errOut, "no such file or directory") {
			t.Errorf("cp into a missing directory = %d %q", code, errOut)
		}
	})
}

func TestExecConnectionLost(t *testing.T) {
	dir, err := os.MkdirTemp("", "bp") // short: macOS caps socket paths at ~104 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	kill := startDaemon(t, dir)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := client.New(dir).CreateSandbox(ctx, daemon.CreateRequest{Name: "box"}); err != nil {
		t.Fatal(err)
	}
	// The fake sandbox's agent runs here, so the command can signal us.
	started := filepath.Join(dir, "started")
	done := make(chan [2]any, 1)
	go func() {
		code, _, errOut := cli(t, dir, "", "exec", "box", "sh", "-c", "touch "+started+"; exec sleep 30")
		done <- [2]any{code, errOut}
	}()
	for _, err := os.Stat(started); err != nil; _, err = os.Stat(started) {
		if ctx.Err() != nil {
			t.Fatal("the command did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	kill()
	select {
	case r := <-done:
		if r[0] != execFailed || !strings.Contains(r[1].(string), "connection lost") {
			t.Errorf("exec after the daemon died = %v %q", r[0], r[1])
		}
	case <-ctx.Done():
		t.Fatal("exec did not exit after the daemon died")
	}
}

// tree describes the files under root: names, types, modes, times,
// contents and link targets.
func tree(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.Walk(root, func(p string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		b.WriteString(rel + " " + info.Mode().String())
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			l, _ := os.Readlink(p)
			b.WriteString(" -> " + l)
		case info.Mode().IsRegular():
			content, _ := os.ReadFile(p)
			fmt.Fprintf(&b, " %v %x", info.ModTime().UTC(), sha256.Sum256(content))
		default:
			fmt.Fprintf(&b, " %v", info.ModTime().UTC())
		}
		b.WriteString("; ")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}
