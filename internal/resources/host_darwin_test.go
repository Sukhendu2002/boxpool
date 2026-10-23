package resources

import "testing"

func TestHostCapacity(t *testing.T) {
	c, err := hostCapacity(t.TempDir())
	if err != nil || len(c.CPUs) == 0 || c.MemoryMB <= 0 || c.AvailableMB < 0 || c.AvailableMB > c.MemoryMB || c.DiskBytes <= 0 {
		t.Fatalf("host capacity: %+v, %v", c, err)
	}
	t.Logf("%+v", c)
}
