// Package artifacts downloads the pinned Firecracker release and guest image
// for this machine into the state directory and verifies them against SHA-256
// checksums compiled into the binary.
package artifacts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// DirName is the artifact cache's directory name inside the state directory.
const DirName = "artifacts"

// Bumping either version means updating the matching checksums and sizes
// below.
const (
	imageVersion = "v0.1.1"
	imageURL     = "https://github.com/Sukhendu2002/boxpool/releases/download/" + imageVersion + "/"
	// Same release and checksums as scripts/bench-boot.sh.
	firecrackerVersion = "v1.17.0"
	firecrackerURL     = "https://github.com/firecracker-microvm/firecracker/releases/download/" + firecrackerVersion + "/"
)

type artifact struct {
	name   string // file name in the cache directory
	url    string
	sha256 string
	size   int64 // exact byte count; caps the download
}

// platform is the artifact set for one GOOS/GOARCH. firecracker is empty on
// macOS, which runs the guest with Apple's Virtualization framework.
type platform struct {
	firecracker, kernel, rootfs, desktop artifact
}

func image(name, sum string, size int64) artifact {
	return artifact{name, imageURL + name, sum, size}
}

func firecracker(arch, sum string, size int64) artifact {
	name := "firecracker-" + firecrackerVersion + "-" + arch + ".tgz"
	return artifact{name, firecrackerURL + name, sum, size}
}

var pinned = map[string]platform{
	"linux/amd64": {
		firecracker: firecracker("x86_64", "06094a1108ae9e82aa4c23a775aa92758f53f1175d422270d9d6162cb9ade558", 7464385),
		kernel:      image("vmlinux-amd64", "6515997fbf84e4f7875065e96dc2066cf535d2d825f0c255706147344ae0d13a", 24451352),
		rootfs:      image("rootfs-amd64.ext4.zst", "0635b600c4e6f7c0b95fe8f04ded322b5886baa398b8898954f21c63ea259adf", 335588678),
		desktop:     image("rootfs-desktop-amd64.ext4.zst", "23a7964afa03ee8fcefa24b0cd3716221b7a3c6c5185652b80b9cf859520886c", 608324341),
	},
	"linux/arm64": {
		firecracker: firecracker("aarch64", "e351ebe4f7a16b5873bbd51005d2e6767103cff4d5ebc829df2d3f95a93e2256", 7294031),
		kernel:      image("vmlinux-arm64", "3bf6f78dd6690fdf58ea7fed4fa5be71233defe9d21b0b1efb77238a2c6c06e1", 18883072),
		rootfs:      image("rootfs-arm64.ext4.zst", "f7338be4c35f19d02f46180da9e7c8debae4d31e5f9f2f2d92c2c946229da025", 329107922),
		desktop:     image("rootfs-desktop-arm64.ext4.zst", "c7952858f2136409f6aeb5a07019aa7779478a3adb07904269573aac56842fd3", 600793647),
	},
	"darwin/arm64": {
		kernel:  image("vmlinux-arm64", "3bf6f78dd6690fdf58ea7fed4fa5be71233defe9d21b0b1efb77238a2c6c06e1", 18883072),
		rootfs:  image("rootfs-arm64.ext4.zst", "f7338be4c35f19d02f46180da9e7c8debae4d31e5f9f2f2d92c2c946229da025", 329107922),
		desktop: image("rootfs-desktop-arm64.ext4.zst", "c7952858f2136409f6aeb5a07019aa7779478a3adb07904269573aac56842fd3", 600793647),
	},
}

func forPlatform(goos, goarch string) ([]artifact, error) {
	p, ok := pinned[goos+"/"+goarch]
	if !ok {
		return nil, fmt.Errorf("no artifacts for %s/%s", goos, goarch)
	}
	return p.baseArtifacts(), nil
}

func (p platform) baseArtifacts() []artifact {
	list := []artifact{p.kernel, p.rootfs}
	if p.firecracker.name != "" {
		list = append([]artifact{p.firecracker}, list...)
	}
	return list
}

// Paths are the local paths of the fetched artifacts, in their published
// form: the Firecracker release tarball and the zstd-compressed ext4 rootfs
// are not unpacked here.
type Paths struct {
	Firecracker string // empty on macOS
	Kernel      string
	Rootfs      string
}

// Fetch downloads the base artifacts into dir/<imageVersion>. Versioned
// directories prevent UnpackOnce from reusing a previous release's rootfs.
func Fetch(ctx context.Context, dir string) (Paths, error) {
	p, ok := pinned[runtime.GOOS+"/"+runtime.GOARCH]
	if !ok {
		return Paths{}, fmt.Errorf("no artifacts for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	return fetchPlatform(ctx, dir, http.DefaultClient, p)
}

func fetchPlatform(ctx context.Context, dir string, client *http.Client, p platform) (Paths, error) {
	dir = filepath.Join(dir, imageVersion)
	if _, err := fetch(ctx, dir, client, p.baseArtifacts()); err != nil {
		return Paths{}, err
	}
	path := func(a artifact) string {
		if a.name == "" {
			return ""
		}
		return filepath.Join(dir, a.name)
	}
	return Paths{Firecracker: path(p.firecracker), Kernel: path(p.kernel), Rootfs: path(p.rootfs)}, nil
}

// DesktopRootfs fetches and unpacks the pinned desktop disk on demand into
// the base image's versioned directory. The base and desktop share a kernel.
func DesktopRootfs(ctx context.Context, dir string) (string, error) {
	p, ok := pinned[runtime.GOOS+"/"+runtime.GOARCH]
	if !ok {
		return "", fmt.Errorf("no desktop artifacts for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	return desktopRootfs(ctx, dir, http.DefaultClient, p.desktop)
}

func desktopRootfs(ctx context.Context, dir string, client *http.Client, a artifact) (string, error) {
	if _, err := fetch(ctx, dir, client, []artifact{a}); err != nil {
		return "", err
	}
	return Rootfs(Paths{Rootfs: filepath.Join(dir, a.name)})
}

// Retry tuning; tests shorten retryDelay.
var (
	attempts        = 3
	retryDelay      = 2 * time.Second
	downloadTimeout = 10 * time.Minute // per attempt; desktop disks are about 600 MB
)

// fetch makes sure each artifact is in dir with its pinned checksum and
// returns the names it had to download.
func fetch(ctx context.Context, dir string, client *http.Client, list []artifact) ([]string, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	var downloaded []string
	for _, a := range list {
		ok, err := verified(filepath.Join(dir, a.name), a)
		if err != nil {
			return downloaded, err
		}
		if ok {
			continue
		}
		if err := download(ctx, dir, client, a); err != nil {
			return downloaded, err
		}
		downloaded = append(downloaded, a.name)
	}
	return downloaded, nil
}

// verified reports whether path exists with a's size and sha256. A file with
// any other content is not trusted and gets downloaded again.
func verified(path string, a artifact) (bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || fi.Size() != a.size {
		return false, err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, fmt.Errorf("verify %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)) == a.sha256, nil
}

// download retries transient failures (network errors, 5xx). There is no
// resume: each attempt starts a fresh temp file.
func download(ctx context.Context, dir string, client *http.Client, a artifact) error {
	for attempt := 1; ; attempt++ {
		retry, err := downloadOnce(ctx, dir, client, a)
		if err == nil || !retry || attempt == attempts || ctx.Err() != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt) * retryDelay):
		}
	}
}

// downloadOnce streams a into a temp file in dir while hashing it, and renames
// it into place only when the checksum matches, so dir never holds a partial
// or unverified file under the artifact's name.
func downloadOnce(ctx context.Context, dir string, client *http.Client, a artifact) (retry bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.url, nil)
	if err != nil {
		return false, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return true, fmt.Errorf("download %s: %w", a.name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode >= 500, fmt.Errorf("download %s: GET %s: %s", a.name, a.url, resp.Status)
	}
	if resp.ContentLength >= 0 && resp.ContentLength != a.size {
		return false, fmt.Errorf("download %s from %s: Content-Length is %d, want %d", a.name, a.url, resp.ContentLength, a.size)
	}

	tmp, err := os.CreateTemp(dir, "."+a.name+".*.part")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	defer tmp.Close()
	h := sha256.New()
	// Read one byte past the pinned size so an oversized or endless body is
	// cut off instead of filling the disk.
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, a.size+1))
	if err != nil {
		return true, fmt.Errorf("download %s: %w", a.name, err)
	}
	if n > a.size {
		return false, fmt.Errorf("download %s from %s: more than the expected %d bytes; discarded it", a.name, a.url, a.size)
	}
	if n < a.size {
		return false, fmt.Errorf("download %s from %s: got %d bytes, want %d; discarded it", a.name, a.url, n, a.size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != a.sha256 {
		return false, fmt.Errorf("download %s from %s: sha256 is %s, want %s; discarded it", a.name, a.url, got, a.sha256)
	}
	if err := tmp.Chmod(0644); err != nil {
		return false, err
	}
	if err := tmp.Sync(); err != nil {
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	return false, os.Rename(tmp.Name(), filepath.Join(dir, a.name))
}
