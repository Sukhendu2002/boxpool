//go:build linux || darwin

package daemon

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func filesURL(srv string, name, path string) string {
	return srv + "/v1/sandboxes/" + name + "/files?path=" + url.QueryEscape(path)
}

func TestFiles(t *testing.T) {
	srv, _ := newAPI(t)
	post(t, srv, `{"name":"web"}`)
	guest := t.TempDir() // the fake sandbox's agent runs here

	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	tw.WriteHeader(&tar.Header{Name: "dir", Typeflag: tar.TypeDir, Mode: 0755})
	tw.WriteHeader(&tar.Header{Name: "dir/hello", Typeflag: tar.TypeReg, Mode: 0644, Size: 3})
	tw.Write([]byte("hi\n"))
	tw.Close()
	req, _ := http.NewRequest(http.MethodPut, filesURL(srv.URL, "web", guest), &b)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT = %d", resp.StatusCode)
	}
	if got, err := os.ReadFile(filepath.Join(guest, "dir", "hello")); string(got) != "hi\n" {
		t.Fatalf("uploaded file = %q, %v", got, err)
	}

	resp, err = http.Get(filesURL(srv.URL, "web", filepath.Join(guest, "dir")))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/x-tar" {
		t.Fatalf("GET = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	var names []string
	tr := tar.NewReader(resp.Body)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
	if strings.Join(names, " ") != "dir dir/hello" {
		t.Errorf("downloaded %q", names)
	}

	// The agent's errors pass through unchanged.
	for _, c := range []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, filepath.Join(guest, "missing"), http.StatusNotFound},
		{http.MethodPut, filepath.Join(guest, "missing"), http.StatusNotFound},
		{http.MethodGet, "", http.StatusBadRequest},
	} {
		req, _ := http.NewRequest(c.method, filesURL(srv.URL, "web", c.path), strings.NewReader(""))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != c.want || !strings.Contains(string(body), `"message"`) {
			t.Errorf("%s %q = %d %s, want %d", c.method, c.path, resp.StatusCode, body, c.want)
		}
	}
}

func TestFilesErrors(t *testing.T) {
	srv, d := newAPI(t)
	_, sb, _ := post(t, srv, `{"name":"web"}`)
	if code, body := call(t, srv, http.MethodGet, "/v1/sandboxes/nope/files?path=/"); code != http.StatusNotFound {
		t.Errorf("GET nope = %d %s", code, body)
	}
	d.Stop(context.Background(), sb.ID)
	if code, body := call(t, srv, http.MethodGet, "/v1/sandboxes/web/files?path=/tmp"); code != http.StatusBadGateway || !strings.Contains(body, "is it running?") {
		t.Errorf("GET on a stopped sandbox = %d %s", code, body)
	}
}

// TestFilesDownloadAborted checks that a download the agent fails midway
// reaches the client as an error, not as a shorter but valid tar.
func TestFilesDownloadAborted(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads unreadable files")
	}
	srv, _ := newAPI(t)
	post(t, srv, `{"name":"web"}`)
	guest := filepath.Join(t.TempDir(), "dir")
	os.Mkdir(guest, 0755)
	os.WriteFile(filepath.Join(guest, "a"), bytes.Repeat([]byte("x"), 1<<20), 0644)
	os.WriteFile(filepath.Join(guest, "b"), nil, 0) // unreadable: fails after a is sent
	resp, err := http.Get(filesURL(srv.URL, "web", guest))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET = %d", resp.StatusCode)
	}
	tr := tar.NewReader(resp.Body)
	for {
		_, err := tr.Next()
		if err == io.EOF {
			t.Fatal("the aborted download read as a complete tar")
		}
		if err != nil {
			break
		}
		if _, err := io.Copy(io.Discard, tr); err != nil {
			break
		}
	}
}
