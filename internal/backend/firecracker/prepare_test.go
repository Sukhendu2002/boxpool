package firecracker

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"

	"boxpool.run/boxpool/internal/artifacts"
)

func writeTgz(t *testing.T, path string, files map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	if err := os.WriteFile(path, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestPrepare(t *testing.T) {
	dir := t.TempDir()
	p := artifacts.Paths{
		Firecracker: filepath.Join(dir, "firecracker-v1.17.0-x86_64.tgz"),
		Kernel:      filepath.Join(dir, "vmlinux-amd64"),
		Rootfs:      filepath.Join(dir, "rootfs-amd64.ext4.zst"),
	}
	writeTgz(t, p.Firecracker, map[string]string{
		"release-v1.17.0-x86_64/jailer-v1.17.0-x86_64":      "jailer",
		"release-v1.17.0-x86_64/firecracker-v1.17.0-x86_64": "firecracker binary",
	})
	// Data, a long zero run and an unaligned tail: the zero run becomes a hole.
	image := make([]byte, 3<<20+100)
	copy(image, "superblock")
	copy(image[2<<20+5:], "data")
	image[len(image)-1] = 1
	enc, _ := zstd.NewWriter(nil)
	if err := os.WriteFile(p.Rootfs, enc.EncodeAll(image, nil), 0644); err != nil {
		t.Fatal(err)
	}

	fc, rootfs, err := prepare(p)
	if err != nil {
		t.Fatal(err)
	}
	if fc != filepath.Join(dir, "firecracker-v1.17.0-x86_64") || rootfs != filepath.Join(dir, "rootfs-amd64.ext4") {
		t.Fatalf("got %s and %s", fc, rootfs)
	}
	if b, _ := os.ReadFile(fc); string(b) != "firecracker binary" {
		t.Errorf("firecracker is %q", b)
	}
	if fi, err := os.Stat(fc); err != nil || fi.Mode().Perm() != 0755 {
		t.Errorf("firecracker mode: %v %v", fi.Mode(), err)
	}
	if b, _ := os.ReadFile(rootfs); !bytes.Equal(b, image) {
		t.Errorf("rootfs differs from the decompressed image (%d bytes, want %d)", len(b), len(image))
	}

	// Present outputs are kept, not unpacked again.
	os.WriteFile(fc, []byte("kept"), 0755)
	if _, _, err := prepare(p); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(fc); string(b) != "kept" {
		t.Errorf("prepare unpacked firecracker again: %q", b)
	}

	// A tarball without the binary is an error and leaves nothing behind.
	os.Remove(fc)
	writeTgz(t, p.Firecracker, map[string]string{"release-v1.17.0-x86_64/jailer-v1.17.0-x86_64": "jailer"})
	if _, _, err := prepare(p); err == nil {
		t.Fatal("prepare succeeded without a firecracker binary")
	}
	if entries, _ := filepath.Glob(filepath.Join(dir, "*firecracker-v1.17.0-x86_64*")); len(entries) != 1 {
		t.Errorf("left behind %v", entries)
	}
}
