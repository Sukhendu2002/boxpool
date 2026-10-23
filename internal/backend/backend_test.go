package backend

import "testing"

func TestSpecDefaults(t *testing.T) {
	got, err := (Spec{Name: "web", Image: "image:v1"}).WithDefaults()
	if err != nil || got != (Spec{Name: "web", Image: "image:v1", CPUs: 2, MemoryMB: 2048, DiskGB: 10}) {
		t.Fatalf("defaults: %+v, %v", got, err)
	}
	explicit := Spec{CPUs: 1, MemoryMB: 512, DiskGB: 3}
	if got, err := explicit.WithDefaults(); err != nil || got != explicit {
		t.Fatalf("explicit limits: %+v, %v", got, err)
	}
	for _, s := range []Spec{{CPUs: -1}, {MemoryMB: -1}, {DiskGB: -1}, {CPUs: int(^uint(0) >> 1)}, {MemoryMB: int(^uint(0) >> 1)}, {DiskGB: int(^uint(0) >> 1)}} {
		if _, err := s.WithDefaults(); err == nil {
			t.Errorf("invalid limits accepted: %+v", s)
		}
	}
}
