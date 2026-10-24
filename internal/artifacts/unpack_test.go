package artifacts

import (
	"bytes"
	"errors"
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
	path, err := Rootfs(p)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(path); err != nil || !bytes.Equal(b, image) {
		t.Fatalf("decompressed image differs: %v", err)
	}
	if err := os.WriteFile(p.Rootfs, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Rootfs(p); err != nil {
		t.Fatalf("unpacked a cached rootfs again: %v", err)
	}
	os.Remove(path)
	if _, err := Rootfs(p); err == nil {
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
	err := UnpackOnce(path, 0755, func(f *os.File) error {
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
