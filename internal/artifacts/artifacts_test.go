package artifacts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
			if !strings.HasSuffix(a.url, "/"+a.name) || !hexSum.MatchString(a.sha256) {
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
	list := []artifact{{"kernel", srv.URL + "/kernel", sum(good)}}

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
		{"good", srv.URL + "/good", sum(good)},
		{"rootfs", srv.URL + "/rootfs", sum(good)},
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
	if _, err := fetch(context.Background(), dir, srv.Client(), []artifact{{"fc", srv.URL + "/fc", sum(good)}}); err != nil {
		t.Fatal(err)
	}
	if n := hits["fc"].Load(); int(n) != attempts {
		t.Errorf("%d requests, want %d", n, attempts)
	}

	// Client errors are not retried.
	_, err := fetch(context.Background(), dir, srv.Client(), []artifact{{"missing", srv.URL + "/missing", sum(good)}})
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("got %v, want a 404 error", err)
	}

	srv2, hits2 := server(t, map[string][]byte{"fc": good}, map[string]int{"fc": attempts})
	_, err = fetch(context.Background(), t.TempDir(), srv2.Client(), []artifact{{"fc", srv2.URL + "/fc", sum(good)}})
	if err == nil || !strings.Contains(err.Error(), "503") || int(hits2["fc"].Load()) != attempts {
		t.Errorf("got %v after %d requests, want a 503 error after %d", err, hits2["fc"].Load(), attempts)
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
