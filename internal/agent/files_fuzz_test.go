package agent

import (
	"archive/tar"
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

// Exercise the HTTP upload boundary, the tar parser and filesystem together.
// Names and link targets are independent: a symlink entry followed by a child
// must never redirect the child's contents outside the upload destination.
func FuzzFileUploadPaths(f *testing.F) {
	for _, seed := range [][2]string{{"file", "outside"}, {"../escape", "../"}, {"/absolute", "/"}, {"a/../../escape", "../outside"}, {"link/escape", "../outside"}, {"a//b", "/tmp"}, {"./file", "missing"}, {"..\\escape", "../outside"}, {"%2e%2e/escape", "/tmp"}, {"a\x00b", "/"}} {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, name, target string) {
		if len(name) > 2048 || len(target) > 2048 {
			t.Skip()
		}
		base := t.TempDir()
		dest := filepath.Join(base, "dest")
		outside := filepath.Join(base, "outside")
		if err := os.Mkdir(dest, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(outside, 0700); err != nil {
			t.Fatal(err)
		}
		canary := filepath.Join(outside, "canary")
		if err := os.WriteFile(canary, []byte("unchanged"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(dest, "existing")); err != nil {
			t.Fatal(err)
		}
		var body bytes.Buffer
		tw := tar.NewWriter(&body)
		for _, h := range []*tar.Header{
			{Name: "link", Linkname: target, Typeflag: tar.TypeSymlink, Mode: 0777},
			{Name: name, Typeflag: tar.TypeReg, Mode: 0600, Size: 1},
		} {
			if err := tw.WriteHeader(h); err != nil {
				return
			}
			if h.Size != 0 {
				if _, err := tw.Write([]byte("x")); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodPut, "/v1/files?path="+url.QueryEscape(dest), &body)
		w := httptest.NewRecorder()
		putFiles(w, r)
		if w.Code != http.StatusNoContent && w.Code != http.StatusBadRequest {
			t.Fatalf("unexpected upload status: %d: %s", w.Code, w.Body.String())
		}
		b, err := os.ReadFile(canary)
		if err != nil || string(b) != "unchanged" {
			t.Fatalf("upload escaped: %q, %v", b, err)
		}
		entries, err := os.ReadDir(base)
		if err != nil || len(entries) != 2 {
			t.Fatalf("upload wrote beside destination: %v, %v", entries, err)
		}
		entries, err = os.ReadDir(outside)
		if err != nil || len(entries) != 1 {
			t.Fatalf("upload followed a symlink outside destination: %v, %v", entries, err)
		}
	})
}
