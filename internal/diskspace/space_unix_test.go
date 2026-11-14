//go:build linux || darwin

package diskspace

import (
	"errors"
	"math"
	"path/filepath"
	"strings"
	"testing"
)

func TestCapacitySelection(t *testing.T) {
	for _, tc := range []struct {
		name          string
		free, native  int64
		nativeErr     error
		wantAvailable int64
	}{
		{"reclaimable", 16_000_000_000, 53_000_000_000, nil, 53_000_000_000},
		{"zero free with reclaimable space", 0, 21_000_000_000, nil, 21_000_000_000},
		{"equal", 16, 16, nil, 16},
		{"lagging estimate", 16, 12, nil, 16},
		{"missing estimate", 16, -1, nil, 16},
		{"zero estimate", 16, 0, nil, 16},
		{"native failure", 16, 53, errors.New("unavailable"), 16},
		{"full filesystem", 0, 0, nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const path = "/volume with spaces/state"
			got, err := probe(path, func(got string) (int64, error) {
				if got != path {
					t.Fatalf("filesystem path = %q", got)
				}
				return tc.free, nil
			}, func(got string) (int64, error) {
				if got != path {
					t.Fatalf("native path = %q", got)
				}
				return tc.native, tc.nativeErr
			})
			if err != nil || got != (Info{FreeBytes: tc.free, AvailableBytes: tc.wantAvailable}) {
				t.Fatalf("probe = %+v, %v", got, err)
			}
		})
	}
}

func TestFilesystemFailure(t *testing.T) {
	want := errors.New("filesystem unavailable")
	got, err := probe("/state", func(string) (int64, error) { return 0, want }, func(string) (int64, error) {
		t.Fatal("native estimate must not hide a filesystem failure")
		return 53_000_000_000, nil
	})
	if !errors.Is(err, want) || got != (Info{}) {
		t.Fatalf("probe = %+v, %v", got, err)
	}
}

func TestBlockBytes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		blocks  uint64
		size    int64
		want    int64
		wantErr bool
	}{
		{"empty", 0, 4096, 0, false},
		{"normal", 10, 4096, 40960, false},
		{"maximum", math.MaxInt64, 1, math.MaxInt64, false},
		{"zero block size", 10, 0, 0, true},
		{"negative block size", 10, -1, 0, true},
		{"overflow", math.MaxUint64, 4096, 0, true},
		{"signed overflow", uint64(math.MaxInt64) + 1, 1, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := blockBytes(tc.blocks, tc.size)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("blockBytes = %d, %v", got, err)
			}
		})
	}
}

func TestProbe(t *testing.T) {
	dir := t.TempDir()
	got, err := Probe(dir)
	if err != nil || got.FreeBytes < 0 || got.AvailableBytes < got.FreeBytes {
		t.Fatalf("Probe = %+v, %v", got, err)
	}
	t.Logf("capacity: %+v", got)
	for _, path := range []string{filepath.Join(dir, "missing"), dir + "\x00other"} {
		if _, err := Probe(path); err == nil {
			t.Errorf("accepted invalid path %q", path)
		}
	}
	if _, err := freeBytes(strings.Repeat("x", 10000)); err == nil {
		t.Error("accepted overlong path")
	}
}
