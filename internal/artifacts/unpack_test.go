package artifacts

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestRootfs(t *testing.T) {
	dir := t.TempDir()
	p := Paths{Rootfs: filepath.Join(dir, "rootfs-arm64.ext4.zst")}
	image := make([]byte, 3<<20+101)
	copy(image, "superblock")
	copy(image[2<<20+7:], "data")
	image[len(image)-1] = 1
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	if err := os.WriteFile(p.Rootfs, enc.EncodeAll(image, nil), 0600); err != nil {
		t.Fatal(err)
	}
	path, err := Rootfs(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(path); err != nil || !bytes.Equal(b, image) {
		t.Fatalf("decompressed image differs: %v", err)
	}
	if err := os.WriteFile(p.Rootfs, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Rootfs(context.Background(), p); err != nil {
		t.Fatalf("unpacked a cached rootfs again: %v", err)
	}
	os.Remove(path)
	if _, err := Rootfs(context.Background(), p); err == nil {
		t.Error("accepted corrupt zstd")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("failed decompression left image: %v", err)
	}
	if files, _ := filepath.Glob(filepath.Join(dir, ".*.part")); len(files) != 0 {
		t.Errorf("failed decompression left temporary files: %v", files)
	}
}

func TestUnpackOnceFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unpacked")
	want := errors.New("failed write")
	err := UnpackOnce(context.Background(), path, 0755, func(f *os.File) error {
		f.WriteString("partial")
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("lost fill error: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("partial output committed: %v", err)
	}
}

type cancelAfterReader struct {
	io.Reader
	cancel       context.CancelFunc
	reads, after int
}

func (r *cancelAfterReader) Read(b []byte) (int, error) {
	n, err := r.Reader.Read(b)
	r.reads++
	if r.reads == r.after {
		r.cancel()
	}
	return n, err
}

func TestUnpackCancellation(t *testing.T) {
	image := bytes.Repeat([]byte{1}, 4<<20)
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	compressed := enc.EncodeAll(image, nil)
	enc.Close()
	for _, mode := range []string{"decompression", "sparse data", "sparse holes"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			dir := t.TempDir()
			path := filepath.Join(dir, "rootfs.ext4")
			input, after := image, 2
			if mode == "decompression" {
				input, after = compressed, 1
			} else if mode == "sparse holes" {
				input = make([]byte, len(image))
			}
			r := &cancelAfterReader{bytes.NewReader(input), cancel, 0, after}
			err := UnpackOnce(ctx, path, 0644, func(f *os.File) error {
				if mode == "decompression" {
					return decompress(ctx, r, f)
				}
				return writeSparse(ctx, f, r)
			})
			if !errors.Is(err, context.Canceled) || r.reads != after {
				t.Fatalf("unpack returned %v after %d reads, want cancellation after %d", err, r.reads, after)
			}
			if files := entries(t, dir); len(files) != 0 {
				t.Fatalf("canceled unpack left output or temporary files: %v", files)
			}
			if err := UnpackOnce(context.Background(), path, 0644, func(f *os.File) error {
				_, err := f.WriteString("complete")
				return err
			}); err != nil {
				t.Fatalf("retry after cancellation: %v", err)
			}
			if b, err := os.ReadFile(path); err != nil || string(b) != "complete" {
				t.Fatalf("retry reused partial data: %q, %v", b, err)
			}
		})
	}
}

func TestUnpackCancelledBeforePublication(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	dir := t.TempDir()
	path := filepath.Join(dir, "rootfs.ext4")
	err := UnpackOnce(ctx, path, 0644, func(f *os.File) error {
		_, err := f.WriteString("partial")
		cancel()
		return err
	})
	if !errors.Is(err, context.Canceled) || len(entries(t, dir)) != 0 {
		t.Fatalf("canceled fill was published: %v", err)
	}
	if err := os.WriteFile(path, []byte("cached"), 0644); err != nil {
		t.Fatal(err)
	}
	err = UnpackOnce(ctx, path, 0644, func(*os.File) error {
		t.Fatal("canceled cache hit called fill")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled cache hit returned success: %v", err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "cached" {
		t.Fatalf("cancellation changed a completed cache entry: %q, %v", b, err)
	}
}
