//go:build linux || (darwin && !cgo)

package diskspace

import "testing"

func TestFilesystemOnly(t *testing.T) {
	got, err := Probe(t.TempDir())
	if err != nil || got.AvailableBytes != got.FreeBytes {
		t.Fatalf("filesystem-only capacity: %+v, %v", got, err)
	}
}
