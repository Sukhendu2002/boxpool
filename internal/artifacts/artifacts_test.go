package artifacts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestForPlatform(t *testing.T) {
	tests := []struct {
		goos, goarch string
		want         []string
		otherArch    []string
	}{
		{"linux", "amd64", []string{"firecracker-v1.17.0-x86_64.tgz", "vmlinux-amd64", "rootfs-amd64.ext4.zst"}, []string{"arm64", "aarch64"}},
		{"linux", "arm64", []string{"firecracker-v1.17.0-aarch64.tgz", "vmlinux-arm64", "rootfs-arm64.ext4.zst"}, []string{"amd64", "x86_64"}},
		{"darwin", "arm64", []string{"vmlinux-arm64", "rootfs-arm64.ext4.zst"}, []string{"amd64", "x86_64", "firecracker"}},
	}
	hexSum := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for _, tt := range tests {
		list, err := forPlatform(tt.goos, tt.goarch)
		if err != nil {
			t.Fatalf("%s/%s: %v", tt.goos, tt.goarch, err)
		}
		var names []string
		for _, a := range list {
			names = append(names, a.name)
			if !strings.HasSuffix(a.url, "/"+a.name) || !hexSum.MatchString(a.sha256) || a.size <= 0 {
				t.Errorf("%s/%s: bad pin %+v", tt.goos, tt.goarch, a)
			}
			for _, other := range tt.otherArch {
				if strings.Contains(a.url, other) {
					t.Errorf("%s/%s selected %s, which is for %s", tt.goos, tt.goarch, a.url, other)
				}
			}
		}
		if !slices.Equal(names, tt.want) {
			t.Errorf("%s/%s: got %v, want %v", tt.goos, tt.goarch, names, tt.want)
		}
		desktop := pinned[tt.goos+"/"+tt.goarch].desktop
		if desktop.name != "rootfs-desktop-"+tt.goarch+".ext4.zst" || desktop.url != imageURL+desktop.name || !hexSum.MatchString(desktop.sha256) || desktop.size <= 0 {
			t.Errorf("%s/%s: bad desktop pin %+v", tt.goos, tt.goarch, desktop)
		}
	}
	for _, p := range [][2]string{{"darwin", "amd64"}, {"windows", "amd64"}, {"linux", "riscv64"}} {
		if _, err := forPlatform(p[0], p[1]); err == nil || !strings.Contains(err.Error(), "no artifacts for "+p[0]+"/"+p[1]) {
			t.Errorf("%s/%s: got err %v", p[0], p[1], err)
		}
	}
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// server serves files by name and counts requests per name. A name in fail
// answers 503 that many times first.
func server(t *testing.T, files map[string][]byte, fail map[string]int) (*httptest.Server, map[string]*atomic.Int32) {
	t.Helper()
	hits := map[string]*atomic.Int32{}
	for name := range files {
		hits[name] = new(atomic.Int32)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		b, ok := files[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if n := hits[name].Add(1); int(n) <= fail[name] {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	retryDelay = 0
	return srv, hits
}

func entries(t *testing.T, dir string) []string {
	t.Helper()
	des, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, de := range des {
		names = append(names, de.Name())
	}
	return names
}

func TestFetchVerifiesAndSkips(t *testing.T) {
	good := []byte("kernel bytes")
	srv, hits := server(t, map[string][]byte{"kernel": good}, nil)
	dir := filepath.Join(t.TempDir(), DirName)
	list := []artifact{{"kernel", srv.URL + "/kernel", sum(good), int64(len(good))}}

	got, err := fetch(context.Background(), dir, srv.Client(), list)
	if err != nil || !slices.Equal(got, []string{"kernel"}) {
		t.Fatalf("first fetch: %v, %v", got, err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "kernel"))
	if err != nil || string(b) != string(good) {
		t.Fatalf("kernel = %q, %v", b, err)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "kernel")); fi.Mode().Perm() != 0644 {
		t.Errorf("mode %v, want 0644", fi.Mode().Perm())
	}

	got, err = fetch(context.Background(), dir, srv.Client(), list)
	if err != nil || len(got) != 0 || hits["kernel"].Load() != 1 {
		t.Fatalf("second fetch downloaded %v (%d requests), %v", got, hits["kernel"].Load(), err)
	}

	// A truncated or corrupted cached file is replaced, not trusted.
	if err := os.WriteFile(filepath.Join(dir, "kernel"), good[:3], 0644); err != nil {
		t.Fatal(err)
	}
	got, err = fetch(context.Background(), dir, srv.Client(), list)
	if err != nil || !slices.Equal(got, []string{"kernel"}) {
		t.Fatalf("fetch after truncation: %v, %v", got, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "kernel")); string(b) != string(good) {
		t.Errorf("kernel = %q after re-download", b)
	}
	if names := entries(t, dir); !slices.Equal(names, []string{"kernel"}) {
		t.Errorf("dir holds %v, want only kernel", names)
	}
}

func TestFetchRejectsCorruptDownload(t *testing.T) {
	good, bad := []byte("rootfs bytes"), []byte("rootfs bytez")
	srv, _ := server(t, map[string][]byte{"good": good, "rootfs": bad}, nil)
	dir := t.TempDir()
	list := []artifact{
		{"good", srv.URL + "/good", sum(good), int64(len(good))},
		{"rootfs", srv.URL + "/rootfs", sum(good), int64(len(good))},
	}
	got, err := fetch(context.Background(), dir, srv.Client(), list)
	if err == nil {
		t.Fatal("corrupt download accepted")
	}
	for _, want := range []string{"rootfs", sum(good), sum(bad)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
	if !slices.Equal(got, []string{"good"}) {
		t.Errorf("downloaded %v, want [good]", got)
	}
	if names := entries(t, dir); !slices.Equal(names, []string{"good"}) {
		t.Errorf("dir holds %v, want only good (corrupt file and temp file deleted)", names)
	}
}

func TestFetchRetries(t *testing.T) {
	good := []byte("tarball")
	srv, hits := server(t, map[string][]byte{"fc": good}, map[string]int{"fc": attempts - 1})
	dir := t.TempDir()
	if _, err := fetch(context.Background(), dir, srv.Client(), []artifact{{"fc", srv.URL + "/fc", sum(good), int64(len(good))}}); err != nil {
		t.Fatal(err)
	}
	if n := hits["fc"].Load(); int(n) != attempts {
		t.Errorf("%d requests, want %d", n, attempts)
	}

	// Client errors are not retried.
	_, err := fetch(context.Background(), dir, srv.Client(), []artifact{{"missing", srv.URL + "/missing", sum(good), int64(len(good))}})
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("got %v, want a 404 error", err)
	}

	srv2, hits2 := server(t, map[string][]byte{"fc": good}, map[string]int{"fc": attempts})
	_, err = fetch(context.Background(), t.TempDir(), srv2.Client(), []artifact{{"fc", srv2.URL + "/fc", sum(good), int64(len(good))}})
	if err == nil || !strings.Contains(err.Error(), "503") || int(hits2["fc"].Load()) != attempts {
		t.Errorf("got %v after %d requests, want a 503 error after %d", err, hits2["fc"].Load(), attempts)
	}
}

// TestFetchRejectsOversizedDownload serves an endless body without a
// Content-Length and checks the client hangs up after size+1 bytes.
func TestFetchRejectsOversizedDownload(t *testing.T) {
	const limit = 64 << 20
	var sent atomic.Int64
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		chunk := make([]byte, 64<<10)
		for sent.Load() < limit {
			n, err := w.Write(chunk)
			sent.Add(int64(n))
			if err != nil {
				return
			}
		}
	}))
	retryDelay = 0
	dir := t.TempDir()
	_, err := fetch(context.Background(), dir, srv.Client(), []artifact{{"kernel", srv.URL + "/kernel", sum([]byte("good")), 4}})
	srv.Close() // waits for the handler, which stops once the client hangs up
	if err == nil || !strings.Contains(err.Error(), "kernel") || !strings.Contains(err.Error(), "more than the expected 4 bytes") {
		t.Errorf("got %v, want an oversize error", err)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("%d requests, want 1 (no retry)", n)
	}
	if n := sent.Load(); n >= limit {
		t.Errorf("server sent all %d bytes; the client did not cut the download off", n)
	}
	if names := entries(t, dir); len(names) != 0 {
		t.Errorf("dir holds %v, want nothing", names)
	}
}

// TestFetchRejectsShortDownload sends one byte too few, chunked so there is no
// Content-Length to catch it first.
func TestFetchRejectsShortDownload(t *testing.T) {
	good := []byte("kernel bytes")
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.(http.Flusher).Flush()
		w.Write(good[:len(good)-1])
	}))
	t.Cleanup(srv.Close)
	retryDelay = 0
	dir := t.TempDir()
	_, err := fetch(context.Background(), dir, srv.Client(), []artifact{{"kernel", srv.URL + "/kernel", sum(good), int64(len(good))}})
	if err == nil || !strings.Contains(err.Error(), "got 11 bytes, want 12") {
		t.Errorf("got %v, want a short-download error", err)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("%d requests, want 1 (no retry)", n)
	}
	if names := entries(t, dir); len(names) != 0 {
		t.Errorf("dir holds %v, want nothing", names)
	}
}

func TestFetchRejectsWrongContentLength(t *testing.T) {
	good := []byte("kernel bytes")
	srv, hits := server(t, map[string][]byte{"kernel": good}, nil)
	dir := t.TempDir()
	_, err := fetch(context.Background(), dir, srv.Client(), []artifact{{"kernel", srv.URL + "/kernel", sum(good), 4}})
	if err == nil || !strings.Contains(err.Error(), "Content-Length is 12, want 4") {
		t.Errorf("got %v, want a Content-Length error", err)
	}
	if n := hits["kernel"].Load(); n != 1 {
		t.Errorf("%d requests, want 1 (no retry)", n)
	}
	if names := entries(t, dir); len(names) != 0 {
		t.Errorf("dir holds %v, want nothing", names)
	}
}

// TestFetchReal downloads the real pinned artifacts for this machine, which
// proves the pins. Set BOXPOOL_HOME to keep them in $BOXPOOL_HOME/artifacts.
func TestFetchReal(t *testing.T) {
	if os.Getenv("BOXPOOL_FETCH_REAL") != "1" {
		t.Skip("set BOXPOOL_FETCH_REAL=1 to download the real artifacts")
	}
	dir := filepath.Join(t.TempDir(), DirName)
	if home := os.Getenv("BOXPOOL_HOME"); home != "" {
		dir = filepath.Join(home, DirName)
	}
	ctx := context.Background()
	paths, err := Fetch(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("fetched %+v", paths)

	list, _ := forPlatform(runtime.GOOS, runtime.GOARCH)
	dir = filepath.Dir(paths.Rootfs)
	got, err := fetch(ctx, dir, http.DefaultClient, list)
	if err != nil || len(got) != 0 {
		t.Fatalf("second run downloaded %v, %v", got, err)
	}
	t.Log("second run downloaded nothing")

	if err := os.Truncate(paths.Kernel, 1024); err != nil {
		t.Fatal(err)
	}
	got, err = fetch(ctx, dir, http.DefaultClient, list)
	if err != nil || !slices.Equal(got, []string{filepath.Base(paths.Kernel)}) {
		t.Fatalf("after truncating %s downloaded %v, %v", paths.Kernel, got, err)
	}
	t.Logf("truncated %s was downloaded again", filepath.Base(paths.Kernel))
}

func TestImageUpgradeCache(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	// The old release unpacked into the top-level artifacts directory.
	legacy := filepath.Join(dir, "rootfs-arm64.ext4")
	if err := os.WriteFile(legacy, []byte("old agent"), 0600); err != nil {
		t.Fatal(err)
	}
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	base, desktop := enc.EncodeAll([]byte("new base agent"), nil), enc.EncodeAll([]byte("new desktop agent"), nil)
	srv, hits := server(t, map[string][]byte{"kernel": []byte("kernel"), "rootfs-arm64.ext4.zst": base, "rootfs-desktop-arm64.ext4.zst": desktop}, nil)
	a := func(name string, b []byte) artifact {
		return artifact{name, srv.URL + "/" + name, sum(b), int64(len(b))}
	}
	p := platform{kernel: a("kernel", []byte("kernel")), rootfs: a("rootfs-arm64.ext4.zst", base), desktop: a("rootfs-desktop-arm64.ext4.zst", desktop)}
	paths, err := fetchPlatform(ctx, dir, srv.Client(), p)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(paths.Rootfs) != filepath.Join(dir, imageVersion) || hits[p.desktop.name].Load() != 0 {
		t.Fatalf("base fetch: %+v; desktop downloads %d", paths, hits[p.desktop.name].Load())
	}
	root, err := Rootfs(paths)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(root); string(b) != "new base agent" {
		t.Fatalf("upgrade reused old rootfs: %q", b)
	}
	if b, _ := os.ReadFile(legacy); string(b) != "old agent" {
		t.Fatalf("legacy rootfs modified: %q", b)
	}
	for range 2 {
		path, err := desktopRootfs(ctx, filepath.Dir(root), srv.Client(), p.desktop)
		if err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(path); string(b) != "new desktop agent" {
			t.Fatalf("desktop image: %q", b)
		}
	}
	if hits[p.desktop.name].Load() != 1 {
		t.Fatalf("desktop downloaded %d times", hits[p.desktop.name].Load())
	}
	// A bad download must never be unpacked, even on the lazy desktop path.
	p.desktop.sha256 = sum([]byte("wrong checksum"))
	if _, err := desktopRootfs(ctx, t.TempDir(), srv.Client(), p.desktop); err == nil {
		t.Fatal("corrupt desktop accepted")
	}
}

func TestDesktopDownloadCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.(http.Flusher).Flush()
		cancel()
		<-r.Context().Done()
	}))
	defer srv.Close()
	dir := t.TempDir()
	a := artifact{"rootfs-desktop.ext4.zst", srv.URL, sum([]byte("unused")), 6}
	if _, err := desktopRootfs(ctx, dir, srv.Client(), a); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled desktop download: %v", err)
	}
	if names := entries(t, dir); len(names) != 0 {
		t.Fatalf("canceled download left files: %v", names)
	}
}
