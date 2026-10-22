//go:build linux || darwin

package agent

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func tarBody(t *testing.T, headers ...*tar.Header) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	for _, h := range headers {
		if err := w.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			if _, err := io.CopyN(w, strings.NewReader(strings.Repeat("x", int(h.Size))), h.Size); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return &b
}

func TestFilesRoundTrip(t *testing.T) {
	c := serve(t)
	ctx := testContext(t)
	src := filepath.Join(t.TempDir(), "source & files")
	if err := os.MkdirAll(filepath.Join(src, "nested"), 0755); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{"run": "#!/bin/sh\necho hello\n", "empty": "", "nested/data": strings.Repeat("streamed\n", 1<<18)} {
		if err := os.WriteFile(filepath.Join(src, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("run", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	mtime := time.Unix(1700000000, 123456789)
	modes := map[string]os.FileMode{".": 0550, "nested": 0750 | os.ModeSticky, "run": 0751 | os.ModeSetuid, "empty": 0640, "nested/data": 0600}
	for name, mode := range modes {
		p := filepath.Join(src, name)
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	if err := setSymlinkMetadata(filepath.Join(src, "link"), 0777, mtime); err != nil {
		t.Fatal(err)
	}
	in, out := t.TempDir(), t.TempDir()
	defer func() {
		for _, root := range []string{src, filepath.Join(in, filepath.Base(src)), filepath.Join(out, filepath.Base(src))} {
			os.Chmod(root, 0755)
			os.Chmod(filepath.Join(root, "nested"), 0755)
		}
	}()
	r, w := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := writeTar(w, src)
		w.CloseWithError(err)
		done <- err
	}()
	defer r.Close()
	if err := c.Upload(ctx, in, r); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	body, err := c.Download(ctx, filepath.Join(in, filepath.Base(src)))
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	if err := extractTar(body, out); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, body); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{in, out} {
		for name, mode := range modes {
			p := filepath.Join(root, filepath.Base(src), name)
			info, err := os.Lstat(p)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode()&(os.ModePerm|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != mode || !info.ModTime().Equal(mtime) {
				t.Errorf("%s: mode %v, mtime %v; want %v, %v", name, info.Mode(), info.ModTime(), mode, mtime)
			}
			if !info.IsDir() {
				want, err := os.ReadFile(filepath.Join(src, name))
				if err != nil {
					t.Fatal(err)
				}
				got, err := os.ReadFile(p)
				if err != nil || !bytes.Equal(got, want) {
					t.Errorf("%s: contents differ: %v", name, err)
				}
			}
		}
		link := filepath.Join(root, filepath.Base(src), "link")
		if target, err := os.Readlink(link); err != nil || target != "run" {
			t.Errorf("Readlink = %q, %v", target, err)
		}
		info, err := os.Lstat(link)
		if err != nil || !info.ModTime().Equal(mtime) {
			t.Errorf("link mtime: %v, %v", info, err)
		}
	}
}

func TestDownloadSingleEntry(t *testing.T) {
	c := serve(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("hello"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"file", "link"} {
		t.Run(name, func(t *testing.T) {
			body, err := c.Download(testContext(t), filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			defer body.Close()
			tr := tar.NewReader(body)
			h, err := tr.Next()
			if err != nil || h.Name != name {
				t.Fatalf("first entry = %v, %v", h, err)
			}
			if name == "link" && (h.Typeflag != tar.TypeSymlink || h.Linkname != "missing") {
				t.Errorf("symlink header = %+v", h)
			}
			if name == "file" {
				b, err := io.ReadAll(tr)
				if err != nil || string(b) != "hello" {
					t.Errorf("file contents = %q, %v", b, err)
				}
			}
			if _, err := tr.Next(); err != io.EOF {
				t.Errorf("next entry = %v, want EOF", err)
			}
		})
	}
}

func TestUploadRejectsEntries(t *testing.T) {
	c := serve(t)
	for _, name := range []string{"../escape", "a/../../escape", "/absolute"} {
		t.Run(name, func(t *testing.T) {
			err := c.Upload(testContext(t), t.TempDir(), tarBody(t, &tar.Header{Name: name, Mode: 0644, Typeflag: tar.TypeReg}))
			if err == nil || !strings.Contains(err.Error(), "400 Bad Request") || !strings.Contains(err.Error(), name) {
				t.Errorf("Upload = %v, want 400 naming %q", err, name)
			}
		})
	}
	for _, typ := range []byte{tar.TypeLink, tar.TypeChar, tar.TypeBlock, tar.TypeFifo, tar.TypeCont} {
		err := c.Upload(testContext(t), t.TempDir(), tarBody(t, &tar.Header{Name: "unsupported", Typeflag: typ, Linkname: "target"}))
		if err == nil || !strings.Contains(err.Error(), "unsupported") || !strings.Contains(err.Error(), "400 Bad Request") {
			t.Errorf("type %d: Upload = %v", typ, err)
		}
	}
}

func TestUploadSymlinkSafety(t *testing.T) {
	c := serve(t)
	outside := t.TempDir()
	victim := filepath.Join(outside, "file")
	if err := os.WriteFile(victim, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, existing := range []bool{false, true} {
		dest := t.TempDir()
		var headers []*tar.Header
		if existing {
			if err := os.Symlink(outside, filepath.Join(dest, "link")); err != nil {
				t.Fatal(err)
			}
		} else {
			headers = append(headers, &tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: outside, ModTime: time.Unix(1700000000, 0)})
		}
		headers = append(headers, &tar.Header{Name: "link/file", Typeflag: tar.TypeReg, Size: 4})
		err := c.Upload(testContext(t), dest, tarBody(t, headers...))
		if err == nil || !strings.Contains(err.Error(), "link/file") {
			t.Errorf("symlink parent accepted: %v", err)
		}
	}
	for _, hardlink := range []bool{false, true} {
		dest := t.TempDir()
		link := filepath.Join(dest, "file")
		var err error
		if hardlink {
			err = os.Link(victim, link)
		} else {
			err = os.Symlink(victim, link)
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Upload(testContext(t), dest, tarBody(t, &tar.Header{Name: "file", Typeflag: tar.TypeReg, Mode: 0600, Size: 4})); err != nil {
			t.Fatal(err)
		}
	}
	if b, err := os.ReadFile(victim); err != nil || string(b) != "keep" {
		t.Errorf("outside file modified: %q, %v", b, err)
	}
	info, err := os.Stat(victim)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Errorf("outside metadata modified: %v, %v", info, err)
	}
}

func TestUploadBadTarAndPartialWrites(t *testing.T) {
	c := serve(t)
	for _, data := range [][]byte{[]byte("not a tar"), tarBody(t, &tar.Header{Name: "truncated", Size: 10}).Bytes()[:515]} {
		err := c.Upload(testContext(t), t.TempDir(), bytes.NewReader(data))
		if err == nil || !strings.Contains(err.Error(), "400 Bad Request") || !strings.Contains(err.Error(), "entry") {
			t.Errorf("bad tar accepted: %v", err)
		}
	}
	dest := t.TempDir()
	err := c.Upload(testContext(t), dest, tarBody(t, &tar.Header{Name: "written", Typeflag: tar.TypeReg, Mode: 0600, Size: 1}, &tar.Header{Name: "../escape", Typeflag: tar.TypeReg}))
	if err == nil {
		t.Fatal("bad entry accepted")
	}
	if b, err := os.ReadFile(filepath.Join(dest, "written")); err != nil || string(b) != "x" {
		t.Errorf("earlier entry = %q, %v", b, err)
	}
}

func TestFilesHTTPErrors(t *testing.T) {
	c := serve(t)
	missing := filepath.Join(t.TempDir(), "missing")
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		for _, p := range []string{"", missing} {
			req, err := http.NewRequestWithContext(testContext(t), method, "http://agent/v1/files?path="+url.QueryEscape(p), nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := c.hc.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			want := http.StatusNotFound
			if p == "" {
				want = http.StatusBadRequest
			}
			var e errorResponse
			err = json.NewDecoder(resp.Body).Decode(&e)
			resp.Body.Close()
			if resp.StatusCode != want || err != nil || e.Message == "" || resp.Header.Get("Content-Type") != "application/json" {
				t.Errorf("%s %q: %s, %+v, %v", method, p, resp.Status, e, err)
			}
		}
	}
	if _, err := c.Download(context.Background(), missing); err == nil || !strings.Contains(err.Error(), "404 Not Found") {
		t.Errorf("Download missing = %v", err)
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := c.Upload(testContext(t), file, tarBody(t)); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("Upload to file = %v", err)
	}
}

func TestTarDotDirectoryMetadata(t *testing.T) {
	dest := t.TempDir()
	defer func() {
		os.Chmod(dest, 0755)
		os.Chmod(filepath.Join(dest, "a"), 0755)
	}()
	mtime := time.Unix(1700000000, 0)
	body := tarBody(t,
		&tar.Header{Name: ".", Typeflag: tar.TypeDir, Mode: 0500, ModTime: mtime},
		&tar.Header{Name: "a", Typeflag: tar.TypeDir, Mode: 0500, ModTime: mtime},
		&tar.Header{Name: "a/file", Typeflag: tar.TypeReg, Mode: 0400, Size: 1, ModTime: mtime},
	)
	if err := extractTar(body, dest); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".", "a", "a/file"} {
		info, err := os.Stat(filepath.Join(dest, name))
		if err != nil || !info.ModTime().Equal(mtime) {
			t.Errorf("%s: metadata = %v, %v", name, info, err)
		}
	}
}

func TestDownloadRejectsUnsupportedEntries(t *testing.T) {
	c := serve(t)
	sock := tempSock(t)
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for _, src := range []string{sock, filepath.Dir(sock)} {
		body, err := c.Download(testContext(t), src)
		if body != nil {
			body.Close()
		}
		if err == nil || !strings.Contains(err.Error(), "400 Bad Request") || !strings.Contains(err.Error(), "agent.sock") || !strings.Contains(err.Error(), "unsupported") {
			t.Errorf("Download %s = %v", src, err)
		}
	}
}
