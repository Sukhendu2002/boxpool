package firecracker

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"boxpool.run/boxpool/internal/artifacts"
)

// prepare unpacks the fetched artifacts next to them, once: the firecracker
// binary from its release tarball and the base rootfs from its zstd file.
func prepare(p artifacts.Paths) (firecracker, rootfs string, err error) {
	if !strings.HasSuffix(p.Firecracker, ".tgz") {
		return "", "", fmt.Errorf("unexpected artifact name %s", p.Firecracker)
	}
	// The release tarball holds release-<v>-<arch>/firecracker-<v>-<arch>.
	firecracker = strings.TrimSuffix(p.Firecracker, ".tgz")
	err = artifacts.UnpackOnce(firecracker, 0755, func(w *os.File) error {
		return extract(p.Firecracker, filepath.Base(firecracker), w)
	})
	if err != nil {
		return "", "", err
	}
	rootfs, err = artifacts.Rootfs(p)
	return firecracker, rootfs, err
}

// extract copies the regular file called name from the gzipped tarball tgz.
func extract(tgz, name string, w io.Writer) error {
	f, err := os.Open(tgz)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("%s not found in %s", name, tgz)
		}
		if err != nil {
			return err
		}
		if h.Typeflag == tar.TypeReg && path.Base(h.Name) == name {
			_, err = io.Copy(w, tr)
			return err
		}
	}
}
