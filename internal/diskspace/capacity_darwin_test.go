//go:build darwin && cgo

package diskspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestImportantCapacity(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "capacity with spaces 雪")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	got, err := importantCapacity(dir)
	if err != nil || got < 0 {
		t.Fatalf("Foundation important-usage capacity: %d, %v", got, err)
	}
	t.Logf("Foundation important-usage capacity: %d bytes", got)
	alias := filepath.Join(t.TempDir(), "relocated state")
	if err := os.Symlink(dir, alias); err != nil {
		t.Fatal(err)
	}
	if n, err := importantCapacity(alias); err != nil || n < 0 {
		t.Fatalf("symlinked directory capacity: %d, %v", n, err)
	}
	if _, err := importantCapacity(filepath.Join(dir, "missing")); err == nil {
		t.Error("native query accepted a missing directory")
	}
}
