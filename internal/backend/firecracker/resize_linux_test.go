//go:build linux

package firecracker

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestResizeRootfs(t *testing.T) {
	for _, tool := range []string{"mkfs.ext4", "resize2fs", "dumpe2fs", "e2fsck"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("needs e2fsprogs: %v", err)
		}
	}
	ctx := context.Background()
	dir := t.TempDir()
	base := filepath.Join(dir, "base.ext4")
	grown := filepath.Join(dir, "grown.ext4")
	f, err := os.Create(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(32 << 20); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if out, err := exec.Command("mkfs.ext4", "-q", "-F", base).CombinedOutput(); err != nil {
		t.Fatalf("mkfs: %v: %s", err, out)
	}
	before, err := os.ReadFile(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := copyRootfs(grown, base); err != nil {
		t.Fatal(err)
	}
	if err := growRootfs(ctx, grown, 1<<30); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(grown); err != nil || fi.Size() != 1<<30 {
		t.Fatalf("grown file: %v %v", fi, err)
	}
	out, err := exec.Command("dumpe2fs", "-h", grown).CombinedOutput()
	if err != nil {
		t.Fatalf("dumpe2fs: %v: %s", err, out)
	}
	// mkfs defaults vary by host. The filesystem itself must fill 1 GiB,
	// rather than leaving only unused bytes at the end of its image.
	var blocks, blockSize int64
	for _, line := range strings.Split(string(out), "\n") {
		key, value, _ := strings.Cut(line, ":")
		if key == "Block count" {
			blocks, _ = strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		}
		if key == "Block size" {
			blockSize, _ = strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		}
	}
	if blocks*blockSize != 1<<30 {
		t.Fatalf("filesystem was not grown: %s", out)
	}
	if out, err := exec.Command("e2fsck", "-fn", grown).CombinedOutput(); err != nil {
		t.Fatalf("grown disk inconsistent: %v: %s", err, out)
	}
	after, err := os.ReadFile(base)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("base image changed: %v", err)
	}
	if err := growRootfs(ctx, grown, 32<<20); err == nil {
		t.Fatal("shrinking succeeded")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := growRootfs(canceled, grown, 2<<30); err == nil {
		t.Fatal("canceled resize succeeded")
	}
}
