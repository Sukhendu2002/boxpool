package artifacts

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// Rootfs decompresses the fetched rootfs next to it, once, and returns
// the path of the ext4 image. Backends copy or clone it per sandbox and
// never write to it.
func Rootfs(ctx context.Context, p Paths) (string, error) {
	if !strings.HasSuffix(p.Rootfs, ".zst") {
		return "", fmt.Errorf("unexpected rootfs artifact name %s", p.Rootfs)
	}
	rootfs := strings.TrimSuffix(p.Rootfs, ".zst")
	return rootfs, UnpackOnce(ctx, rootfs, 0644, func(w *os.File) error {
		in, err := os.Open(p.Rootfs)
		if err != nil {
			return err
		}
		defer in.Close()
		return decompress(ctx, in, w)
	})
}

// UnpackOnce creates dst with fill unless it exists. fill writes a temp file
// that is renamed into place, so dst is never left partial.
func UnpackOnce(ctx context.Context, dst string, mode os.FileMode, fill func(*os.File) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := os.Stat(dst); !errors.Is(err, fs.ErrNotExist) {
		if canceled := ctx.Err(); canceled != nil {
			return canceled
		}
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
	if err := ctx.Err(); err != nil {
		return err
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
	if err := ctx.Err(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

func decompress(ctx context.Context, in io.Reader, f *os.File) error {
	d, err := zstd.NewReader(contextReader{ctx, in})
	if err != nil {
		return err
	}
	defer d.Close()
	return writeSparse(ctx, f, d)
}

type contextReader struct {
	ctx context.Context
	io.Reader
}

func (r contextReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.Reader.Read(b)
	if canceled := r.ctx.Err(); canceled != nil {
		return n, canceled
	}
	return n, err
}

const blockSize = 4096

// writeSparse copies r to f and skips all-zero blocks, leaving holes. The
// ext4 images contain large unused regions; sandbox copies keep these holes.
func writeSparse(ctx context.Context, f *os.File, r io.Reader) error {
	var zero [blockSize]byte
	isZero := func(b []byte) bool { return bytes.Equal(b, zero[:len(b)]) }
	buf := make([]byte, 256*blockSize)
	var off int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := io.ReadFull(r, buf)
		if canceled := ctx.Err(); canceled != nil {
			return canceled
		}
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
