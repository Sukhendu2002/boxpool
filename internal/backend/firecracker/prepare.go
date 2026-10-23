package firecracker

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"

	"boxpool.run/boxpool/internal/artifacts"
)

// prepare unpacks the fetched artifacts next to them, once: the firecracker
// binary from its release tarball and the base rootfs from its zstd file.
func prepare(p artifacts.Paths) (firecracker, rootfs string, err error) {
	if !strings.HasSuffix(p.Firecracker, ".tgz") || !strings.HasSuffix(p.Rootfs, ".zst") {
		return "", "", fmt.Errorf("unexpected artifact names %s and %s", p.Firecracker, p.Rootfs)
	}
	// The release tarball holds release-<v>-<arch>/firecracker-<v>-<arch>.
	firecracker = strings.TrimSuffix(p.Firecracker, ".tgz")
	err = unpackOnce(firecracker, 0755, func(w *os.File) error {
		return extract(p.Firecracker, filepath.Base(firecracker), w)
	})
	if err != nil {
		return "", "", err
	}
	rootfs = strings.TrimSuffix(p.Rootfs, ".zst")
	err = unpackOnce(rootfs, 0644, func(w *os.File) error { return decompress(p.Rootfs, w) })
	return firecracker, rootfs, err
}

// unpackOnce creates path with fill unless it exists. fill writes a temp file
// that is renamed into place, so path is never left partial.
func unpackOnce(dst string, mode os.FileMode, fill func(*os.File) error) error {
	if _, err := os.Stat(dst); !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".*.part")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	defer tmp.Close()
	if err := fill(tmp); err != nil {
		return fmt.Errorf("unpack %s: %w", filepath.Base(dst), err)
	}
	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
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

func decompress(zst string, f *os.File) error {
	in, err := os.Open(zst)
	if err != nil {
		return err
	}
	defer in.Close()
	d, err := zstd.NewReader(in)
	if err != nil {
		return err
	}
	defer d.Close()
	return writeSparse(f, d)
}

const blockSize = 4096

// writeSparse copies r to f and skips all-zero blocks, leaving holes. The
// image is a 512 MiB ext4 file system holding far less data, and a sandbox's
// copy keeps the holes.
func writeSparse(f *os.File, r io.Reader) error {
	var zero [blockSize]byte
	isZero := func(b []byte) bool { return bytes.Equal(b, zero[:len(b)]) }
	buf := make([]byte, 256*blockSize)
	var off int64
	for {
		n, err := io.ReadFull(r, buf)
		for i := 0; i < n; {
			// Write each run of non-zero blocks with one call.
			j := i
			for j < n && !isZero(buf[j:min(j+blockSize, n)]) {
				j = min(j+blockSize, n)
			}
			if j == i {
				i = min(i+blockSize, n)
				continue
			}
			if _, err := f.WriteAt(buf[i:j], off+int64(i)); err != nil {
				return err
			}
			i = j
		}
		off += int64(n)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return f.Truncate(off)
		}
		if err != nil {
			return err
		}
	}
}
