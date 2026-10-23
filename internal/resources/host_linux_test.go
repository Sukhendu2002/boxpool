//go:build linux

package resources

import "testing"

func TestMemoryInfo(t *testing.T) {
	if total, avail, err := memoryInfo("MemTotal: 2097152 kB\nMemAvailable: 1048576 kB\n"); err != nil || total != 2048 || avail != 1024 {
		t.Fatalf("memory info: %d %d %v", total, avail, err)
	}
	for _, text := range []string{"", "MemTotal: 1024 kB", "MemTotal: -1 kB\nMemAvailable: 0 kB", "MemTotal: 1024 B\nMemAvailable: 0 kB", "MemTotal: 1024 kB\nMemAvailable: 2048 kB"} {
		if _, _, err := memoryInfo(text); err == nil {
			t.Errorf("accepted %q", text)
		}
	}
}

func TestHostCapacity(t *testing.T) {
	c, err := hostCapacity(t.TempDir())
	if err != nil || len(c.CPUs) == 0 || c.MemoryMB <= 0 || c.AvailableMB < 0 || c.AvailableMB > c.MemoryMB || c.DiskBytes <= 0 {
		t.Fatalf("host capacity: %+v, %v", c, err)
	}
}
