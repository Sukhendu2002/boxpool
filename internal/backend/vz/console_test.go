package vz

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"boxpool.run/boxpool/internal/backend"
)

func TestCopyConsole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console.log")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	data := append(bytes.Repeat([]byte("x"), 4<<20), []byte("last message\n")...)
	if err := copyConsole(f, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, p := range []string{path, path + ".1"} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		total += fi.Size()
	}
	if total > 2*(backend.ConsoleMax+32<<10) {
		t.Errorf("console burst grew logs to %d bytes", total)
	}
	b, err := os.ReadFile(path)
	if err != nil || !bytes.HasSuffix(b, []byte("last message\n")) {
		t.Errorf("lost console tail: %v", err)
	}
}

func TestCopyConsoleDrainsAfterWriteError(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "console")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	in := bytes.NewReader(bytes.Repeat([]byte("x"), 2<<20))
	if err := copyConsole(f, in); err == nil {
		t.Error("missing write error")
	}
	if n, _ := io.Copy(io.Discard, in); n != 0 {
		t.Errorf("left %d bytes unread after write error", n)
	}
}
