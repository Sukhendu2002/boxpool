//go:build linux

package firecracker

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestCopyRootfs(t *testing.T) {
	for _, tc := range []struct {
		name  string
		clone func(int, int) error
	}{
		{"file system's own", unix.IoctlFileClone}, // a reflink or the fallback
		{"fallback", func(int, int) error { return unix.EOPNOTSUPP }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cloneFile = tc.clone
			t.Cleanup(func() { cloneFile = unix.IoctlFileClone })
			dir := t.TempDir()
			src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
			f, err := os.Create(src)
			if err != nil {
				t.Fatal(err)
			}
			f.WriteAt([]byte("head"), 0)
			f.WriteAt([]byte("middle"), 32<<20)
			f.Truncate(64 << 20) // ends in a hole
			f.Close()

			if err := copyRootfs(dst, src); err != nil {
				t.Fatal(err)
			}
			want, _ := os.ReadFile(src)
			got, _ := os.ReadFile(dst)
			if !bytes.Equal(got, want) {
				t.Fatalf("copy differs: %d bytes, want %d", len(got), len(want))
			}
			var st syscall.Stat_t
			if err := syscall.Stat(dst, &st); err != nil {
				t.Fatal(err)
			}
			if used := st.Blocks * 512; used > 4<<20 {
				t.Errorf("copy uses %d bytes on disk; holes were filled", used)
			}
			if err := copyRootfs(dst, src); err == nil {
				t.Error("copy over an existing file succeeded")
			}
		})
	}
}

func TestCopyRootfsError(t *testing.T) {
	cloneFile = func(int, int) error { return unix.EIO }
	t.Cleanup(func() { cloneFile = unix.IoctlFileClone })
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	os.WriteFile(src, []byte("x"), 0600)
	if err := copyRootfs(dst, src); err == nil {
		t.Fatal("an I/O error from the clone was ignored")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("failed copy left %s: %v", dst, err)
	}
}
