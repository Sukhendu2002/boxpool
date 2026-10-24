package artifacts

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// Rootfs decompresses the fetched base rootfs next to it, once, and returns
// the path of the ext4 image. Backends copy or clone it per sandbox and
// never write to it.
func Rootfs(p Paths) (string, error) {
	if !strings.HasSuffix(p.Rootfs, ".zst") {
		return "", fmt.Errorf("unexpected rootfs artifact name %s", p.Rootfs)
	}
	rootfs := strings.TrimSuffix(p.Rootfs, ".zst")
	return rootfs, UnpackOnce(rootfs, 0644, func(w *os.File) error { return decompress(p.Rootfs, w) })
}

// UnpackOnce creates dst with fill unless it exists. fill writes a temp file
// that is renamed into place, so dst is never left partial.
func UnpackOnce(dst string, mode os.FileMode, fill func(*os.File) error) error {
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
