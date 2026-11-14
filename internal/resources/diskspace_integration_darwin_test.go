//go:build darwin && cgo && integration

package resources

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"boxpool.run/boxpool/internal/backend"
	"boxpool.run/boxpool/internal/diskspace"
)

// Exercise the real host probe when APFS has enough reclaimable capacity.
// This writes only a reservation manifest, not a large disk or an actual VM.
func TestIntegrationReclaimableCapacity(t *testing.T) {
	dir := t.TempDir()
	space, err := diskspace.Probe(dir)
	if err != nil {
		t.Fatal(err)
	}
	const gib = int64(1 << 30)
	diskGB := space.FreeBytes/gib + 1
	request := diskGB * gib
	// Leave headroom for estimates changing between the two host probes.
	if space.AvailableBytes-request < gib {
		t.Skip("filesystem has insufficient reclaimable headroom for this check")
	}
	c, err := hostCapacity(dir)
	if err != nil {
		t.Fatal(err)
	}
	cpus, memory, _, _ := free(c, nil)
	if cpus < 1 || memory < 512 {
		t.Skip("host lacks spare CPU/memory for an admission check")
	}
	m := Manager{StateDir: dir}
	a, err := m.Create(context.Background(), "vz", "reclaimable", backend.Spec{CPUs: 1, MemoryMB: 512, DiskGB: int(diskGB)}, request)
	if err != nil {
		t.Fatalf("admit %d bytes with capacity %+v: %v", request, space, err)
	}
	if a.DiskBytes != request {
		t.Fatalf("disk reservation = %d, want %d", a.DiskBytes, request)
	}
	if _, err := os.Stat(filepath.Join(dir, "sandboxes", "reclaimable", manifest)); err != nil {
		t.Fatal(err)
	}
	t.Logf("reserved %d bytes; immediately free %d; available %d", request, space.FreeBytes, space.AvailableBytes)
}
